package serverapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/submission"
)

// Timeouts of the submission calls (docs/api/submissions.md). A plan can
// hold every minute row of 92 days on several devices, and an upload is up
// to 32 MiB on a slow school network, so these are longer than the
// daemon's per-call timeout. The *http.Client given to New must allow them
// (its Timeout 0 or at least UploadTimeout).
const (
	PlanTimeout   = 3 * time.Minute
	UploadTimeout = 20 * time.Minute
)

// maxPlanBytes bounds a plan response: the largest body the server
// accepts back is the same order of size.
const maxPlanBytes = submission.MaxDecompressedBytes

// Why a submission call failed, for errors.Is. Each wraps the server's
// *Error too, so errors.As still finds the status and message.
var (
	// ErrBanned is 403: the user's linked Hackatime account is banned.
	ErrBanned = errors.New("your Hackatime account is banned, so you can't submit hours")
	// ErrRefused is 400: the server refused the request itself (a range
	// it can't take, a block it holds no evidence day for, bad JSON).
	ErrRefused = errors.New("the server refused the submission")
	// ErrNotFound is 404: an unknown program or submission.
	ErrNotFound = errors.New("not found on the server")
	// ErrConflict is 409: nothing to submit, a decision already recorded,
	// or this device already added its part.
	ErrConflict = errors.New("the submission conflicts with what the server holds")
	// ErrTooLarge is 413, or a body over the limits before sending.
	ErrTooLarge = errors.New("the submission is too large; submit a shorter range")
	// ErrKeyRejected is 401: the API key was rejected.
	ErrKeyRejected = errors.New("the server rejected the API key; run `logbook login`")
)

// SubmitError is a failed submission call: Reason is one of the Err*
// values above (nil when the call may simply be retried later: a network
// error, 429 or 5xx), and Cause is the call's *Error.
type SubmitError struct {
	Reason error
	Cause  *Error
}

func (e *SubmitError) Error() string {
	switch {
	case e.Reason != nil && e.Cause.Message != "" && e.Cause.Message != e.Reason.Error():
		return fmt.Sprintf("%v: %s", e.Reason, e.Cause.Message)
	case e.Reason != nil:
		return e.Reason.Error()
	case e.Cause.Status == http.StatusTooManyRequests:
		msg := "too many submissions; try again later"
		if e.Cause.RetryAfter > 0 {
			msg = fmt.Sprintf("too many submissions; try again in %s", e.Cause.RetryAfter.Round(time.Second))
		}
		return msg
	}
	return fmt.Sprintf("%v (nothing was submitted; try again later)", e.Cause)
}

// Unwrap returns the reason and the cause.
func (e *SubmitError) Unwrap() []error {
	if e.Reason == nil {
		return []error{e.Cause}
	}
	return []error{e.Reason, e.Cause}
}

// submitError maps a failed call's *Error to a *SubmitError. Only answers
// with the server's JSON error body get a reason (classify): a school
// filter's HTML 403 isn't a ban.
func submitError(err error) error {
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	out := &SubmitError{Cause: e}
	switch {
	case e.Kind == KindAuth:
		out.Reason = ErrKeyRejected
	case e.Kind == KindRevoked: // 403 with a JSON error
		out.Reason = ErrBanned
	case e.Kind == KindUnknownDevice: // 404 with a JSON error
		out.Reason = ErrNotFound
	case e.Kind == KindConflict:
		out.Reason = ErrConflict
	case e.Kind == KindInvalid && e.Status == http.StatusRequestEntityTooLarge:
		out.Reason = ErrTooLarge
	case e.Kind == KindInvalid:
		out.Reason = ErrRefused
	}
	return out
}

// PlanQuery is GET /api/v1/me/submissions/plan's query.
type PlanQuery struct {
	Project  string
	From, To string // YYYY-MM-DD, inclusive
	// DeviceID is the agent's own device, so the plan marks it
	// this_device.
	DeviceID string
	// Submission is an existing submission's id, or "": the plan is then
	// its frozen rows (project and range must be the submission's).
	Submission string
}

// Plan fetches the minute rows the server holds for a project and range,
// from every device (submission.Plan), or with q.Submission the rows that
// submission froze.
func (c *Client) Plan(ctx context.Context, t Target, q PlanQuery) (submission.Plan, error) {
	ctx, cancel := context.WithTimeout(ctx, PlanTimeout)
	defer cancel()
	v := url.Values{"project": {q.Project}, "from": {q.From}, "to": {q.To}}
	if q.DeviceID != "" {
		v.Set("device", q.DeviceID)
	}
	if q.Submission != "" {
		v.Set("submission", q.Submission)
	}
	req, err := c.request(ctx, t, http.MethodGet, "/api/v1/me/submissions/plan?"+v.Encode(), nil)
	if err != nil {
		return submission.Plan{}, err
	}
	var plan submission.Plan
	if err := c.do(req, maxPlanBytes, &plan, http.StatusOK); err != nil {
		return submission.Plan{}, submitError(err)
	}
	if plan.Project != q.Project || plan.From != q.From || plan.To != q.To {
		return submission.Plan{}, &Error{Kind: KindRetry, Status: http.StatusOK,
			err: fmt.Errorf("the server answered with a plan for %q %s to %s", plan.Project, plan.From, plan.To)}
	}
	return plan, nil
}

// Submitted is the server's answer to an upload.
type Submitted struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// ReviewURL is the review link; it may be empty for an added part.
	ReviewURL string `json:"review_url"`
}

// Submit uploads a submission: body is the JSON of a submission.Upload,
// sent gzip-compressed (POST /api/v1/submissions).
func (c *Client) Submit(ctx context.Context, t Target, body []byte) (Submitted, error) {
	out, err := c.upload(ctx, t, "/api/v1/submissions", body)
	if err == nil && out.ID == "" {
		return Submitted{}, &SubmitError{Cause: &Error{Kind: KindRetry,
			err: errors.New("the server accepted the upload without a submission id")}}
	}
	return out, err
}

// AddPart uploads this device's part of an existing submission: body is
// the JSON of a submission.Part (POST /api/v1/submissions/{id}/parts).
// The server may answer with no body; the id is then the one given.
func (c *Client) AddPart(ctx context.Context, t Target, id string, body []byte) (Submitted, error) {
	out, err := c.upload(ctx, t, "/api/v1/submissions/"+url.PathEscape(id)+"/parts", body)
	if err == nil && out.ID == "" {
		out.ID = id
	}
	return out, err
}

func (c *Client) upload(ctx context.Context, t Target, path string, body []byte) (Submitted, error) {
	if len(body) > submission.MaxDecompressedBytes {
		return Submitted{}, &SubmitError{Reason: ErrTooLarge, Cause: &Error{Kind: KindInvalid,
			Message: fmt.Sprintf("%d MiB uncompressed, at most %d MiB", len(body)>>20, submission.MaxDecompressedBytes>>20)}}
	}
	gz, err := Gzip(body)
	if err != nil {
		return Submitted{}, err
	}
	if len(gz) > submission.MaxCompressedBytes {
		return Submitted{}, &SubmitError{Reason: ErrTooLarge, Cause: &Error{Kind: KindInvalid,
			Message: fmt.Sprintf("%d MiB compressed, at most %d MiB", len(gz)>>20, submission.MaxCompressedBytes>>20)}}
	}
	ctx, cancel := context.WithTimeout(ctx, UploadTimeout)
	defer cancel()
	req, err := c.request(ctx, t, http.MethodPost, path, gz)
	if err != nil {
		return Submitted{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	var out Submitted
	if err := c.do(req, maxResponseBytes, &out, http.StatusAccepted, http.StatusOK, http.StatusCreated, http.StatusNoContent); err != nil {
		return Submitted{}, submitError(err)
	}
	return out, nil
}

// Gzip compresses b.
func Gzip(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, fmt.Errorf("compressing: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compressing: %w", err)
	}
	return buf.Bytes(), nil
}

// request builds an authenticated request; body may be nil.
func (c *Client) request(ctx context.Context, t Target, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.BaseURL, "/")+path, r)
	if err != nil {
		return nil, &Error{Kind: KindRetry, err: fmt.Errorf("invalid server URL: %w", redactURLError(err))}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	return req, nil
}

// do sends req and decodes a successful answer of at most limit bytes into
// out (an empty body leaves out as it is). It returns an *Error.
func (c *Client) do(req *http.Request, limit int64, out any, ok ...int) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Kind: KindRetry, err: fmt.Errorf("sending request: %w", redactURLError(err))}
	}
	defer func() { _ = resp.Body.Close() }()
	for _, s := range ok {
		if resp.StatusCode != s {
			continue
		}
		dec := json.NewDecoder(io.LimitReader(resp.Body, limit))
		if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return &Error{Kind: KindRetry, Status: s,
				err: fmt.Errorf("unexpected response from server (status %d): %w", s, err)}
		}
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &Error{Kind: KindRetry, Status: resp.StatusCode, err: fmt.Errorf("reading response: %w", err)}
	}
	return classify(resp.StatusCode, resp.Header.Get("Retry-After"), raw, time.Now())
}
