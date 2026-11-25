package admin_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/admin"
	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// Only a live admin key of a user whose role is admin gets in; everything
// else is 401 with Hackatime's message, and reads nothing. (The router's
// tests send real sessions, user API keys and OAuth tokens.)
func TestOnlyLiveAdminKeysGetIn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	target := "/api/admin/v1/user/info?id=" + strconv.FormatInt(f.orpheusID, 10)
	f.expect(t, call{target: target}, http.StatusOK)
	reads := len(audit(t, f.st, "admin.read"))

	refused := map[string]call{
		"no credentials":       {target: target, key: "-"},
		"a user API key":       {target: target, key: "7f9c1a52-3b4d-4e6f-8a9b-0c1d2e3f4a5b"},
		"an OAuth token":       {target: target, key: "lbo_" + strings.Repeat("A", 43)},
		"an unknown admin key": {target: target, key: admin.KeyPrefix + strings.Repeat("A", 43)},
		"a key with a known prefix but the wrong rest": {target: target, key: f.key[:16] + strings.Repeat("A", 31)},
		"Basic with the key":                           {target: target, auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(f.key+":"))},
		"Token scheme":                                 {target: target, auth: "Token " + f.key},
		"the key in the query":                         {target: target + "&api_key=" + f.key, key: "-"},
		"a session cookie":                             {target: target, key: "-", cookie: &http.Cookie{Name: "logbook_session", Value: f.key}},
		"the key with a suffix":                        {target: target, key: f.key + "x"},
	}
	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, c)
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "HTTP Token: Access denied.") {
				t.Fatalf("= %d %s, want 401", rec.Code, rec.Body)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("no WWW-Authenticate header")
			}
		})
	}
	if n := len(audit(t, f.st, "admin.read")); n != reads {
		t.Fatalf("refused requests wrote %d admin.read rows", n-reads)
	}

	// A user who isn't an admin can't be given a key.
	if _, err := f.keys.CreateKey(ctx, "orpheus", "nope"); !errors.Is(err, admin.ErrNotAdmin) {
		t.Fatalf("key for a non-admin: %v", err)
	}

	// A revoked key stops working; another key of the same admin doesn't.
	second, err := f.keys.CreateKey(ctx, "fiona", "second")
	if err != nil {
		t.Fatal(err)
	}
	f.expect(t, call{target: target, key: second.Key}, http.StatusOK)
	if err := f.keys.RevokeKey(ctx, second.Prefix); err != nil {
		t.Fatal(err)
	}
	f.expect(t, call{target: target, key: second.Key}, http.StatusUnauthorized)
	f.expect(t, call{target: target}, http.StatusOK)

	// A role changed outside the command line still shuts the key out.
	exec(t, f.st, `UPDATE users SET role = 'user' WHERE id = $1`, f.fiona)
	f.expect(t, call{target: target}, http.StatusUnauthorized)
	exec(t, f.st, `UPDATE users SET role = 'admin' WHERE id = $1`, f.fiona)
	f.expect(t, call{target: target}, http.StatusOK)

	// Revoking the role revokes every key: granting it again doesn't
	// bring them back.
	if n, err := f.keys.Revoke(ctx, "fiona"); err != nil || n != 1 {
		t.Fatalf("revoke = %d, %v; want the one live key", n, err)
	}
	f.expect(t, call{target: target}, http.StatusUnauthorized)
	if _, err := f.keys.Grant(ctx, "fiona"); err != nil {
		t.Fatal(err)
	}
	f.expect(t, call{target: target}, http.StatusUnauthorized)
}

// Each key has a burst of 300 requests, refilled at 300 a minute; other
// keys keep their own budget.
func TestRateLimitPerKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	clock := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	f.mux = http.NewServeMux()
	limiter := admin.NewRateLimiter().WithClock(func() time.Time { return clock })
	admin.NewHandler(admin.NewService(f.st, nil, nil), f.keys, admin.HandlerOptions{Limiter: limiter}).Register(f.mux)

	// The catch-all is the cheapest authenticated path.
	for i := range admin.RateBurst {
		if rec := f.do(t, call{target: "/api/admin/v1/nothing"}); rec.Code != http.StatusNotFound {
			t.Fatalf("request %d = %d", i, rec.Code)
		}
	}
	rec := f.do(t, call{target: "/api/admin/v1/nothing"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("request %d = %d (Retry-After %q), want 429", admin.RateBurst+1, rec.Code, rec.Header().Get("Retry-After"))
	}
	other, err := f.keys.CreateKey(ctx, "fiona", "other")
	if err != nil {
		t.Fatal(err)
	}
	f.expect(t, call{target: "/api/admin/v1/nothing", key: other.Key}, http.StatusNotFound)
	clock = clock.Add(time.Second) // 300 a minute: 5 a second
	for range 5 {
		f.expect(t, call{target: "/api/admin/v1/nothing"}, http.StatusNotFound)
	}
	f.expect(t, call{target: "/api/admin/v1/nothing"}, http.StatusTooManyRequests)
}

// Failed authentications are limited per client, so guessing costs time.
func TestFailedAuthenticationsAreLimited(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.mux = http.NewServeMux()
	fails := httpapi.NewRateLimiter(0.5, 3, 10, time.Minute).WithClock(func() time.Time { return testNow })
	admin.NewHandler(admin.NewService(f.st, nil, nil), f.keys, admin.HandlerOptions{FailLimiter: fails}).Register(f.mux)
	for range 3 {
		f.expect(t, call{target: "/api/admin/v1/user/info?id=1", key: "-"}, http.StatusUnauthorized)
	}
	f.expect(t, call{target: "/api/admin/v1/user/info?id=1", key: "-"}, http.StatusTooManyRequests)
	// A valid key is never held back by the client's failures.
	f.expect(t, call{target: "/api/admin/v1/user/info?id=1"}, http.StatusNotFound)
}
