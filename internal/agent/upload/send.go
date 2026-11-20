package upload

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/wire"
)

// maxResponseBytes bounds how much of a response body is read.
const maxResponseBytes = 1 << 20

// outcome classifies one bulk request.
type outcome int

const (
	// outcomeOK: the request was answered per item; see the item lists.
	outcomeOK outcome = iota
	// outcomeAuth: the key was rejected (401/403); pause until it changes.
	outcomeAuth
	// outcomeBatchRejected: the server refused the request body itself (413,
	// or 400 with a JSON error, as both the Logbook server and Hackatime
	// answer for a malformed or oversized batch). The worker splits the
	// batch, and drops an item only when it is refused on its own.
	outcomeBatchRejected
	// outcomeRetry: anything else that isn't a per-item answer: network
	// errors, 429, 5xx, redirects, other 4xx (a wrong URL, a proxy), or a
	// 2xx that isn't a bulk answer (a captive portal). Nothing is deleted.
	outcomeRetry
)

// result is what one bulk request did.
type result struct {
	outcome outcome
	// done are ids to delete because they were accepted (201/202/409).
	done []int64
	// rejected are ids to delete because the item itself was invalid (4xx).
	rejected []int64
	// retry is how many items must be sent again (429/5xx per item).
	retry int
	// retryAfter is the server's Retry-After, if any.
	retryAfter time.Duration
	// status is the HTTP status (0 for network errors).
	status int
	err    error
}

// send posts one batch to t and classifies the answer.
func send(ctx context.Context, client *http.Client, userAgent string, t Target, items []localdb.Item) result {
	hbs := make([]wire.Heartbeat, len(items))
	for i, it := range items {
		hbs[i] = it.Heartbeat
	}
	body, err := json.Marshal(hbs)
	if err != nil {
		return result{outcome: outcomeRetry, err: fmt.Errorf("encoding batch: %w", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		// Targets validate their URL, so this is a misconfiguration; keep
		// the queue and surface it.
		return result{outcome: outcomeRetry, err: fmt.Errorf("invalid upload URL: %w", redactURLError(err))}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	// wakatime-cli sends the key as Basic base64(key).
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(t.APIKey)))
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	if m := commonMachine(hbs); m != "" {
		req.Header.Set("X-Machine-Name", url.QueryEscape(m))
	}

	resp, err := client.Do(req)
	if err != nil {
		return result{outcome: outcomeRetry, err: fmt.Errorf("sending batch: %w", redactURLError(err))}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return result{outcome: outcomeRetry, status: resp.StatusCode, err: fmt.Errorf("reading response: %w", err)}
	}
	return classify(resp.StatusCode, resp.Header.Get("Retry-After"), raw, items, time.Now())
}

// classify maps a bulk response onto the batch. It is pure so it can be
// tested against every status code. Only a per-item answer deletes items:
// both the Logbook server and Hackatime always answer heartbeats.bulk with
// one [body, status] pair per heartbeat, so anything else (a captive
// portal's 200 page, a proxy's 404) must keep the batch.
func classify(status int, retryAfter string, body []byte, items []localdb.Item, now time.Time) result {
	r := result{status: status, retryAfter: parseRetryAfter(retryAfter, now)}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		r.outcome = outcomeAuth
		r.err = fmt.Errorf("server answered %d: re-login needed", status)
		return r
	case status == http.StatusTooManyRequests || status >= 500:
		r.outcome = outcomeRetry
		r.err = fmt.Errorf("server answered %d%s", status, errorSuffix(body))
		return r
	case status == http.StatusRequestEntityTooLarge,
		status == http.StatusBadRequest && errorSuffix(body) != "":
		r.outcome = outcomeBatchRejected
		r.err = fmt.Errorf("server rejected the batch with %d%s", status, errorSuffix(body))
		return r
	case status >= 300 && status < 400:
		r.outcome = outcomeRetry
		r.err = fmt.Errorf("server redirected the upload (%d); a captive portal or proxy may be in the way", status)
		return r
	case status < 200 || status >= 300:
		r.outcome = outcomeRetry
		r.err = fmt.Errorf("server answered %d%s; check the server URL", status, errorSuffix(body))
		return r
	}

	var parsed struct {
		Responses []json.RawMessage `json:"responses"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Responses) != len(items) {
		r.outcome = outcomeRetry
		r.err = fmt.Errorf("unexpected response from server (status %d, not a heartbeat bulk answer)", status)
		return r
	}
	r.outcome = outcomeOK
	for i, raw := range parsed.Responses {
		code := itemStatus(raw)
		switch {
		case code >= 200 && code < 300, code == http.StatusConflict:
			r.done = append(r.done, items[i].ID)
		case code == http.StatusTooManyRequests || code >= 500 || code == 0:
			r.retry++
		default:
			r.rejected = append(r.rejected, items[i].ID)
		}
	}
	if r.retry > 0 {
		r.err = fmt.Errorf("server asked to retry %d of %d heartbeats", r.retry, len(items))
	}
	return r
}

// itemStatus reads the status from one [body, status] pair (0 if malformed).
func itemStatus(raw json.RawMessage) int {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) < 2 {
		return 0
	}
	var code int
	if err := json.Unmarshal(pair[1], &code); err != nil {
		return 0
	}
	return code
}

// errorSuffix extracts a short, printable error message from a JSON body.
func errorSuffix(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return ""
	}
	msg := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, e.Error)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return ": " + msg
}

// parseRetryAfter reads a Retry-After header (seconds or HTTP date).
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		if s < 0 {
			return 0
		}
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// commonMachine returns the machine name if every heartbeat shares one.
func commonMachine(hbs []wire.Heartbeat) string {
	if len(hbs) == 0 {
		return ""
	}
	m := hbs[0].Machine
	for _, h := range hbs[1:] {
		if h.Machine != m {
			return ""
		}
	}
	return m
}

// redactURLError drops the URL from a *url.Error. URLs hold no keys here,
// but the bare cause reads better in status output.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
