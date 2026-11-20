package serverapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// maxResponseBytes bounds how much of a response body is read.
const maxResponseBytes = 1 << 20

// Target is the server and credentials a call goes to, resolved by the
// caller on every attempt so a new `logbook login` applies at once.
type Target struct {
	// BaseURL is the server's base URL, e.g. https://api.logbook.example.
	BaseURL string
	APIKey  string
}

// Client calls the crediting endpoints. Create it with New.
type Client struct {
	http      *http.Client
	userAgent string
}

// New returns a Client using c, which must have a timeout (the daemon's
// shared client does), and sending userAgent.
func New(c *http.Client, userAgent string) *Client {
	return &Client{http: c, userAgent: userAgent}
}

// Kind classifies a failed call by what the caller should do about it.
type Kind int

const (
	// KindRetry means network error, 429, 5xx, a redirect, or any answer that
	// isn't the endpoint's (a captive portal, a proxy). Retry with backoff.
	KindRetry Kind = iota
	// KindAuth means 401, the API key was rejected. Pause until it changes.
	KindAuth
	// KindRevoked means 403 with a JSON error, the device was revoked.
	KindRevoked
	// KindUnknownDevice means 404 with a JSON error, the server doesn't know the
	// device for this user.
	KindUnknownDevice
	// KindConflict means 409 with a JSON error, the request conflicts with
	// what the server stores. A 409 without one (a proxy) is KindRetry.
	KindConflict
	// KindInvalid means 400, 413 or 422 with a JSON error, the server refused the
	// request itself. Sending it again won't help.
	KindInvalid
)

// Error is a failed call.
type Error struct {
	Kind Kind
	// Status is the HTTP status, or 0 for network errors.
	Status int
	// Message is the server's JSON error message, sanitised and truncated.
	Message string
	// RetryAfter is the server's Retry-After, if any.
	RetryAfter time.Duration
	err        error
}

func (e *Error) Error() string {
	switch {
	case e.err != nil:
		return e.err.Error()
	case e.Message != "":
		return fmt.Sprintf("server answered %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("server answered %d", e.Status)
}

func (e *Error) Unwrap() error { return e.err }

// KindOf returns err's Kind; errors that aren't *Error are KindRetry.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindRetry
}

// RetryAfterOf returns err's Retry-After, or 0.
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// Server error messages the agent acts on. The server sends only
// {"error": message}, so these texts are part of its contract with the
// agent (internal/server/devices and internal/server/credited).
const (
	// keyTakenMessage is the 409 for a public key registered to another
	// account. Any other 409 from registration (the active-device limit)
	// must not make the agent replace its key.
	keyTakenMessage = "public key is registered to another account"
	// batchSizeMarker is in the 400 for a minutes batch over the size limit.
	batchSizeMarker = "minutes per request"
)

// IsKeyTaken reports whether err is the server saying the device's public
// key is registered to another account.
func IsKeyTaken(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindConflict && e.Message == keyTakenMessage
}

// IsTooLarge reports whether err is the server refusing a request for its
// size (413, or the 400 for too many minutes), so a smaller one may pass.
func IsTooLarge(err error) bool {
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindInvalid {
		return false
	}
	return e.Status == http.StatusRequestEntityTooLarge ||
		e.Status == http.StatusBadRequest && strings.Contains(e.Message, batchSizeMarker)
}

// DeviceRequest is the body of POST /api/v1/devices.
type DeviceRequest struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	// PublicKey is the 32-byte ed25519 public key; JSON encodes it as
	// standard base64.
	PublicKey []byte `json:"public_key"`
}

// RegisterDevice registers the device's public key and returns the device
// id. Registering the same key again returns the same id.
func (c *Client) RegisterDevice(ctx context.Context, t Target, d DeviceRequest) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	status, err := c.post(ctx, t, "/api/v1/devices", d, &resp, http.StatusOK, http.StatusCreated)
	if err != nil {
		return "", err
	}
	if _, perr := uuid.Parse(resp.ID); perr != nil || len(resp.ID) != 36 {
		return "", &Error{Kind: KindRetry, Status: status, err: fmt.Errorf("server answered %d without a device id", status)}
	}
	return strings.ToLower(resp.ID), nil
}

// Minute is one credited minute in a minutes upload.
type Minute struct {
	// Minute is UTC on a whole minute; JSON encodes it as RFC 3339.
	Minute   time.Time `json:"minute"`
	Project  string    `json:"project"`
	Label    string    `json:"label"`
	Credited int       `json:"credited"`
	Cuts     []string  `json:"cuts"`
	Flags    []string  `json:"flags"`
}

// MinutesRequest is the body of POST /api/v1/devices/{id}/minutes.
type MinutesRequest struct {
	RulesVersion string   `json:"rules_version"`
	Minutes      []Minute `json:"minutes"`
}

// MaxMinutes is the most minutes one request may carry.
const MaxMinutes = 1000

// ItemError is a minute the server refused, by its index in the request.
type ItemError struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

// MinutesResult is the server's per-item answer.
type MinutesResult struct {
	Accepted int         `json:"accepted"`
	Rejected []ItemError `json:"rejected"`
}

// UploadMinutes upserts credited minutes for the device.
func (c *Client) UploadMinutes(ctx context.Context, t Target, deviceID string, req MinutesRequest) (MinutesResult, error) {
	var res MinutesResult
	status, err := c.post(ctx, t, "/api/v1/devices/"+url.PathEscape(deviceID)+"/minutes", req, &res, http.StatusOK)
	if err != nil {
		return MinutesResult{}, err
	}
	if res.Accepted < 0 || res.Accepted+len(res.Rejected) != len(req.Minutes) {
		return MinutesResult{}, &Error{Kind: KindRetry, Status: status,
			err: fmt.Errorf("server answered %d with %d accepted and %d rejected for %d minutes",
				status, res.Accepted, len(res.Rejected), len(req.Minutes))}
	}
	for _, r := range res.Rejected {
		if r.Index < 0 || r.Index >= len(req.Minutes) {
			return MinutesResult{}, &Error{Kind: KindRetry, Status: status,
				err: fmt.Errorf("server rejected minute %d of %d", r.Index, len(req.Minutes))}
		}
	}
	return res, nil
}

// EvidenceDayRequest is the body of POST /api/v1/devices/{id}/evidence-days.
type EvidenceDayRequest struct {
	Day           string `json:"day"`
	PrevChainHash string `json:"prev_chain_hash"`
	ContentHash   string `json:"content_hash"`
	ChainHash     string `json:"chain_hash"`
	// Signature is the ed25519 signature; JSON encodes it as standard base64.
	Signature    []byte         `json:"signature"`
	RulesVersion string         `json:"rules_version"`
	EventCounts  map[string]int `json:"event_counts"`
}

// UploadEvidenceDay uploads one link of the device's evidence chain. An
// identical re-upload succeeds.
func (c *Client) UploadEvidenceDay(ctx context.Context, t Target, deviceID string, req EvidenceDayRequest) error {
	_, err := c.post(ctx, t, "/api/v1/devices/"+url.PathEscape(deviceID)+"/evidence-days", req, nil,
		http.StatusOK, http.StatusCreated)
	return err
}

// post sends body as JSON and decodes a successful answer into out (unless
// nil). It returns the status, or an *Error.
func (c *Client) post(ctx context.Context, t Target, path string, body, out any, ok ...int) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, &Error{Kind: KindInvalid, err: fmt.Errorf("encoding request: %w", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(t.BaseURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return 0, &Error{Kind: KindRetry, err: fmt.Errorf("invalid server URL: %w", redactURLError(err))}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, &Error{Kind: KindRetry, err: fmt.Errorf("sending request: %w", redactURLError(err))}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, &Error{Kind: KindRetry, Status: resp.StatusCode, err: fmt.Errorf("reading response: %w", err)}
	}
	for _, s := range ok {
		if resp.StatusCode != s {
			continue
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return s, &Error{Kind: KindRetry, Status: s,
					err: fmt.Errorf("unexpected response from server (status %d, not JSON)", s)}
			}
		}
		return s, nil
	}
	return resp.StatusCode, classify(resp.StatusCode, resp.Header.Get("Retry-After"), raw, time.Now())
}

// classify maps a non-success answer to an *Error. Answers that only the
// Logbook server would give (a JSON {"error"} body) are trusted to mean
// what they say; anything else, such as a school filter's HTML 403, is
// retried.
func classify(status int, retryAfter string, body []byte, now time.Time) *Error {
	msg, isJSON := errorMessage(body)
	e := &Error{Kind: KindRetry, Status: status, Message: msg, RetryAfter: parseRetryAfter(retryAfter, now)}
	switch {
	case status == http.StatusUnauthorized:
		e.Kind = KindAuth
	case status == http.StatusForbidden && isJSON:
		e.Kind = KindRevoked
	case status == http.StatusNotFound && isJSON:
		e.Kind = KindUnknownDevice
	case status == http.StatusConflict && isJSON:
		e.Kind = KindConflict
	case (status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge ||
		status == http.StatusUnprocessableEntity) && isJSON:
		e.Kind = KindInvalid
	case status >= 300 && status < 400:
		e.err = fmt.Errorf("server redirected the request (%d); a captive portal or proxy may be in the way", status)
	}
	return e
}

// errorMessage extracts a printable message from a JSON {"error": "..."}
// body, and reports whether the body had that shape.
func errorMessage(body []byte) (string, bool) {
	var e struct {
		Error *string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == nil {
		return "", false
	}
	msg := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, *e.Error)
	if len(msg) > 200 {
		msg = strings.ToValidUTF8(msg[:200], "")
	}
	return msg, true
}

// parseRetryAfter reads a Retry-After header (seconds or HTTP date).
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(s, 0)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// redactURLError drops the URL from a *url.Error so messages stay short.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
