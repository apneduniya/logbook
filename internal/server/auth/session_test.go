package auth_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/auth"
)

func TestSessionEndpoint(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(req{target: "/api/v1/session"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
	garbage := &http.Cookie{Name: auth.SessionCookie, Value: "not-a-session"}
	if rec := e.do(req{target: "/api/v1/session", cookies: []*http.Cookie{garbage}}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("garbage cookie status = %d, want 401", rec.Code)
	}
	b := e.login(fiona)
	if b.csrf == "" || b.userID == "" {
		t.Fatalf("session response missing fields: %+v", b)
	}
	if rec := b.get(e, "/api/v1/session"); rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("session response is cacheable")
	}
}

func TestSessionExpiresAndSlides(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)

	// Activity within the TTL slides the expiry forward...
	for range 3 {
		e.clock.Advance(testTTL * 2 / 3)
		rec := b.get(e, "/api/v1/session")
		if rec.Code != http.StatusOK {
			t.Fatalf("active session expired: %d", rec.Code)
		}
		if c := cookieNamed(rec, auth.SessionCookie); c == nil || c.MaxAge <= 0 {
			t.Fatalf("sliding refresh did not re-issue the cookie: %+v", c)
		}
	}
	// ...and an idle session expires.
	e.clock.Advance(testTTL + time.Second)
	if rec := b.get(e, "/api/v1/session"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("idle session status = %d, want 401", rec.Code)
	}
	n, err := e.svc.CleanupExpiredSessions(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("CleanupExpiredSessions = %d, %v; want 1", n, err)
	}
	if left := e.count("SELECT count(*) FROM sessions"); left != 0 {
		t.Fatalf("sessions left = %d", left)
	}
}

func TestRunSessionCleanupToleratesZeroInterval(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // run one pass, then return
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.RunSessionCleanup(ctx, 0) // a zero interval used to panic in time.NewTicker
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunSessionCleanup did not return after cancellation")
	}
}

func TestSessionTouchIsThrottled(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	e.clock.Advance(time.Minute)
	if rec := b.get(e, "/api/v1/session"); cookieNamed(rec, auth.SessionCookie) != nil {
		t.Fatal("session touched again within the throttle window")
	}
}

func TestCSRFEnforcement(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	body := `{"name":"laptop"}`
	tests := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"missing token", nil, http.StatusForbidden},
		{"wrong token", map[string]string{auth.CSRFHeader: b.csrf + "x"}, http.StatusForbidden},
		{"empty token", map[string]string{auth.CSRFHeader: ""}, http.StatusForbidden},
		{"correct token", map[string]string{auth.CSRFHeader: b.csrf}, http.StatusCreated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(req{method: http.MethodPost, target: "/api/v1/api_keys", body: body,
				cookies: []*http.Cookie{b.cookie}, header: tc.header})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
	// Another user's CSRF token does not work with this session.
	other := e.login(fakeIdentity{ID: "ident!other", FirstName: "Other"})
	rec := e.do(req{method: http.MethodPost, target: "/api/v1/api_keys", body: body,
		cookies: []*http.Cookie{b.cookie}, header: map[string]string{auth.CSRFHeader: other.csrf}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-session CSRF token status = %d, want 403", rec.Code)
	}
}

func TestOptionalSessionIgnoresSessionWithoutCSRF(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	var out struct{ User, Via string }
	decode(t, e.do(req{method: http.MethodPost, target: "/api/v1/probe/optional", cookies: []*http.Cookie{b.cookie}}), &out)
	if out.User != "" {
		t.Fatalf("unsafe request without CSRF ran as %q", out.User)
	}
	decode(t, b.send(e, http.MethodPost, "/api/v1/probe/optional", ""), &out)
	if out.User != b.name || out.Via != "session" {
		t.Fatalf("with CSRF got %+v", out)
	}
}

// TestLogout covers both sign-out routes: POST /auth/logout and DELETE
// /api/v1/session, which the web UI uses because CORS covers /api/ only.
func TestLogout(t *testing.T) {
	for _, rt := range []struct{ method, target string }{
		{http.MethodPost, "/auth/logout"},
		{http.MethodDelete, "/api/v1/session"},
	} {
		t.Run(rt.method+" "+rt.target, func(t *testing.T) {
			e := newEnv(t)
			b := e.login(fiona)
			if rec := e.do(req{method: rt.method, target: rt.target, cookies: []*http.Cookie{b.cookie}}); rec.Code != http.StatusForbidden {
				t.Fatalf("logout without CSRF = %d, want 403", rec.Code)
			}
			rec := b.send(e, rt.method, rt.target, "")
			if rec.Code != http.StatusNoContent {
				t.Fatalf("logout = %d", rec.Code)
			}
			if c := cookieNamed(rec, auth.SessionCookie); c == nil || c.MaxAge >= 0 {
				t.Fatalf("session cookie not cleared: %+v", c)
			}
			if rec := b.get(e, "/api/v1/session"); rec.Code != http.StatusUnauthorized {
				t.Fatalf("session after logout = %d", rec.Code)
			}
			if n := e.count("SELECT count(*) FROM audit_log WHERE action = 'auth.logout'"); n != 1 {
				t.Fatalf("logout audit rows = %d", n)
			}
		})
	}
}
