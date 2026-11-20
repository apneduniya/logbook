package hackatime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// DefaultBaseURL is the public Hackatime instance.
const DefaultBaseURL = "https://hackatime.hackclub.com"

// UserAgent identifies Logbook to Hackatime. It carries a project URL, never
// a person's contact details.
const UserAgent = "Logbook (+https://boogles.dev)"

// Response size limits. Hackatime's heartbeat endpoint has no pagination, so
// a busy day can be large; the importer splits the window when it is too big.
// 8 MiB is several thousand heartbeats: a normal day fits in one response, and
// it is far more than a 15-minute window can plausibly need.
const (
	maxSmallBody             = 64 << 10 // me
	maxSpansBody             = 2 << 20  // a month of activity spans is tens of KiB
	defaultMaxHeartbeatsBody = 8 << 20
)

// Errors callers handle.
var (
	// ErrUnauthorized means Hackatime rejected the token (revoked or expired).
	ErrUnauthorized = errors.New("hackatime: token rejected")
	// ErrNotFound means the requested resource doesn't exist.
	ErrNotFound = errors.New("hackatime: not found")
	// ErrResponseTooLarge means the response exceeded the size limit.
	ErrResponseTooLarge = errors.New("hackatime: response too large")
	// ErrInvalidResponse means the response didn't decode or validate.
	ErrInvalidResponse = errors.New("hackatime: invalid response")
	// ErrNotConfigured means OAuth client credentials are missing.
	ErrNotConfigured = errors.New("hackatime: oauth client not configured")
	// ErrAuthorizationRejected means the token endpoint refused the code
	// (expired, reused, or PKCE verifier mismatch).
	ErrAuthorizationRejected = errors.New("hackatime: authorization rejected")
)

// StatusError is an unexpected HTTP status from Hackatime.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("hackatime: unexpected status %d", e.Code) }

// RateLimitedError means Hackatime asked us to wait longer than the client
// is willing to block. Callers (job workers) should come back after RetryAfter.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("hackatime: rate limited, retry after %s", e.RetryAfter)
}

// ClientConfig configures a Client. Zero values get safe defaults.
type ClientConfig struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	// RedirectURL is Logbook's OAuth callback, e.g. https://logbook.example/auth/hackatime/callback.
	RedirectURL string

	// HTTPClient is used for every request. Its Timeout is ignored in favour
	// of RequestTimeout; its transport gets the Logbook User-Agent.
	HTTPClient *http.Client
	// RequestTimeout bounds each attempt, including reading the body. Default 30s.
	RequestTimeout time.Duration
	// MaxRetries is the number of retries after the first attempt on
	// 429/5xx/network errors. Default 3; negative disables retries.
	MaxRetries int
	// MinInterval is the sustained spacing of all requests from this
	// process. Hackatime allows 10,000 API requests per hour per IP, and
	// every Logbook user shares the server's IP. Default 500ms (7,200/hour).
	// Ignored when Limiter is set.
	MinInterval time.Duration
	// Burst is how many requests may go out back to back after a quiet
	// spell, on top of the sustained rate. Default 2. Ignored when Limiter
	// is set.
	Burst int
	// Limiter, if set, replaces the limiter built from MinInterval and
	// Burst. Tests inject one with a virtual clock.
	Limiter *Limiter
	// MaxRetryWait is the longest the client blocks for a single retry.
	// A longer Retry-After is returned as *RateLimitedError. Default 30s.
	MaxRetryWait time.Duration
	// MaxHeartbeatsBody caps one heartbeats response. Default 8 MiB.
	MaxHeartbeatsBody int64
}

// Client calls Hackatime. It is safe for concurrent use.
type Client struct {
	base         *url.URL
	http         *http.Client
	oauth        *oauth2.Config
	clientID     string
	clientSecret string
	timeout      time.Duration
	maxRetries   int
	maxRetryWait time.Duration
	maxHBBody    int64
	limit        *Limiter // the process-wide request budget
}

// NewClient validates cfg and builds a client.
func NewClient(cfg ClientConfig) (*Client, error) {
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || base.Host == "" || !allowedScheme(base) {
		return nil, fmt.Errorf("hackatime: base URL %q must be an absolute https URL", raw)
	}
	base.RawQuery, base.Fragment = "", ""

	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		cp := *cfg.HTTPClient
		hc = &cp
	}
	hc.Timeout = 0 // per-attempt contexts bound every request
	hc.Transport = userAgentTransport{base: hc.Transport}
	// Never follow redirects off Hackatime with a bearer token attached.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	c := &Client{
		base:         base,
		http:         hc,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		timeout:      orDefault(cfg.RequestTimeout, 30*time.Second),
		maxRetries:   cfg.MaxRetries,
		maxRetryWait: orDefault(cfg.MaxRetryWait, 30*time.Second),
		maxHBBody:    cfg.MaxHeartbeatsBody,
		limit:        cfg.Limiter,
	}
	if c.limit == nil {
		c.limit = NewLimiter(LimiterConfig{Interval: cfg.MinInterval, Burst: cfg.Burst, MaxPause: c.maxRetryWait})
	}
	if c.maxHBBody <= 0 {
		c.maxHBBody = defaultMaxHeartbeatsBody
	}
	if cfg.MaxRetries == 0 {
		c.maxRetries = 3
	} else if cfg.MaxRetries < 0 {
		c.maxRetries = 0
	}
	if cfg.ClientID != "" && cfg.ClientSecret != "" && cfg.RedirectURL != "" {
		c.oauth = &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   c.endpoint("/oauth/authorize"),
				TokenURL:  c.endpoint("/oauth/token"),
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: cfg.RedirectURL,
			Scopes:      []string{"profile", "read"},
		}
	}
	return c, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// allowedScheme accepts https, or plain http to a loopback host (tests and
// local development only).
func allowedScheme(u *url.URL) bool {
	return u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname()))
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) endpoint(path string) string {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String()
}

// getJSON fetches path and decodes the JSON body into out, retrying on
// 429/5xx/network errors. token, if set, is sent as a bearer token.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, token string, maxBody int64, out any) error {
	u := c.endpoint(path)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		body, status, retryAfter, err := c.fetch(ctx, u, token, maxBody)
		retryable := false
		var rl *RateLimitedError
		switch {
		case err != nil:
			// A long pause from the limiter is returned as is: the caller
			// snoozes rather than retrying here.
			if ctx.Err() != nil || errors.Is(err, ErrResponseTooLarge) || errors.As(err, &rl) {
				return err
			}
			retryable = true // network error or attempt timeout
		case status == http.StatusOK:
			return decodeStrict(body, out)
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return ErrUnauthorized
		case status == http.StatusNotFound:
			return ErrNotFound
		case status == http.StatusTooManyRequests || status >= 500:
			retryable = true
			err = &StatusError{Code: status}
		default:
			return &StatusError{Code: status}
		}
		if status == http.StatusTooManyRequests {
			wait := retryAfter
			if wait <= 0 {
				wait = c.backoff(attempt)
			}
			// Hackatime's limit is per IP, so every caller backs off, not
			// just this one. The limiter holds the retry until the pause ends.
			c.limit.Pause(wait)
			if attempt >= c.maxRetries || wait > c.maxRetryWait {
				return &RateLimitedError{RetryAfter: wait}
			}
			continue
		}
		if !retryable || attempt >= c.maxRetries {
			return err
		}
		if err := sleep(ctx, c.backoff(attempt)); err != nil {
			return err
		}
	}
}

// fetch performs one GET attempt and reads the bounded body.
func (c *Client) fetch(ctx context.Context, u, token string, maxBody int64) (body []byte, status int, retryAfter time.Duration, err error) {
	if err := c.limit.Wait(ctx); err != nil {
		return nil, 0, 0, err
	}
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("hackatime: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error includes the URL, which holds no secrets (tokens go in headers).
		return nil, 0, 0, fmt.Errorf("hackatime: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // read fully below; close error carries nothing useful
	retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	body, err = readBounded(resp.Body, maxBody)
	if err != nil {
		return nil, resp.StatusCode, retryAfter, err
	}
	return body, resp.StatusCode, retryAfter, nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("hackatime: reading response: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, ErrResponseTooLarge
	}
	return b, nil
}

// decodeStrict decodes exactly one JSON value into a typed struct. Wrong
// types and trailing data are rejected. Unknown fields are ignored on
// purpose: Hackatime renders whole ActiveRecord rows and adds columns over
// time, and we must not copy fields we don't know (such as IP addresses).
func decodeStrict(body []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after JSON value", ErrInvalidResponse)
	}
	return nil
}

// backoff is exponential with jitter: ~0.5s, 1s, 2s, 4s ...
func (c *Client) backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << min(attempt, 6)
	return d + rand.N(d/2+1) //nolint:gosec // jitter, not security sensitive
}

// parseRetryAfter reads delta-seconds or an HTTP date. It returns 0 when absent or invalid.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(min(secs, 24*3600)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), 24*time.Hour)
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// userAgentTransport sets Logbook's User-Agent on every request.
type userAgentTransport struct{ base http.RoundTripper }

func (t userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", UserAgent)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}
