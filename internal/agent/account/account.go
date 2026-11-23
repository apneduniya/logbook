package account

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// verifyTimeout bounds one verification request.
const verifyTimeout = 10 * time.Second

// ErrKeyRejected means the server answered 401 or 403 for the key.
var ErrKeyRejected = errors.New("the server rejected this API key")

// ErrNotAPI means the server URL answered, but not with the Logbook API.
var ErrNotAPI = errors.New("not a Logbook API")

// Verification is the server's answer to a key check.
type Verification struct {
	// ServerTime is the server's Date header (zero if absent).
	ServerTime time.Time
}

// Verify checks key against the Logbook server at serverURL. It returns
// ErrKeyRejected for an invalid key, and another error if the server could
// not be reached or answered unexpectedly.
func Verify(ctx context.Context, client *http.Client, serverURL, key, userAgent string) (Verification, error) {
	if strings.TrimSpace(key) == "" {
		return Verification{}, ErrKeyRejected
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	u := strings.TrimRight(serverURL, "/") + "/api/v1/users/current/statusbar/today"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Verification{}, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(key)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Verification{}, fmt.Errorf("reaching %s: %w", serverURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection is reused
	v := Verification{}
	if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		v.ServerTime = d
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return v, ErrKeyRejected
	case resp.StatusCode != http.StatusOK:
		return v, fmt.Errorf("server answered %d", resp.StatusCode)
	}
	// A static site (such as the web UI's origin) answers every path with 200
	// and HTML; the API answers with a JSON object holding "data".
	var reply struct {
		Data *json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &reply) != nil || reply.Data == nil {
		return v, fmt.Errorf("%s answered, but it is %w (is this the website's address instead of the API's?)", serverURL, ErrNotAPI)
	}
	return v, nil
}
