package keycheck

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// requestTimeout bounds the whole check, including a slow TLS handshake on
// an old laptop.
const requestTimeout = 15 * time.Second

// ErrMalformed means the key is not a Logbook key (a lowercase UUIDv4).
var ErrMalformed = errors.New("that is not a Logbook API key (expected xxxxxxxx-xxxx-4xxx-xxxx-xxxxxxxxxxxx); copy it from your Logbook settings")

// ErrRejected means the server does not know the key.
var ErrRejected = errors.New("the Logbook server rejected this API key; check you copied the whole key and that it hasn't been revoked")

var keyPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Normalize trims and lowercases key and checks its format.
func Normalize(key string) (string, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if !keyPattern.MatchString(key) {
		return "", ErrMalformed
	}
	return key, nil
}

// Checker asks the server whether a key is valid.
type Checker struct {
	client    *http.Client
	userAgent string
}

// New returns a Checker. The client is reused; userAgent identifies the
// agent version.
func New(client *http.Client, userAgent string) *Checker {
	return &Checker{client: client, userAgent: userAgent}
}

// secureScheme allows https, and plain http only to this machine.
func secureScheme(u *url.URL) bool {
	return u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname()))
}

// isLoopback reports whether host is this machine, where plain http can't
// leak the key.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Check calls GET <server>/api/v1/users/current/statusbar/today, the
// cheapest route that requires an API key, with the key sent as
// wakatime-cli sends it (Basic base64(key)).
func (c *Checker) Check(ctx context.Context, serverURL, key string) error {
	u, err := url.Parse(strings.TrimRight(serverURL, "/") + "/api/v1/users/current/statusbar/today")
	if err != nil || u.Host == "" || !secureScheme(u) {
		return fmt.Errorf("server URL %q is not valid: use https (plain http only for localhost)", serverURL)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("building key check request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(key)))
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()                      // body drained below; close errors don't matter
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // drain so the connection is reused
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrRejected
	default:
		return fmt.Errorf("checking key with %s: unexpected status %d", u.Host, resp.StatusCode)
	}
}
