package auth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/server/auth"
)

var fiona = fakeIdentity{ID: "ident!fiona", FirstName: "Fiona", LastName: "Hackworth", Email: "Fiona@Example.com"}

func TestLoginRedirectsToHCAWithStateAndPKCE(t *testing.T) {
	e := newEnv(t)
	ap := e.startLogin("")

	if got, want := ap.location.Scheme+"://"+ap.location.Host+ap.location.Path, e.hca.URL+"/oauth/authorize"; got != want {
		t.Errorf("authorize URL = %q, want %q", got, want)
	}
	q := ap.location.Query()
	checks := map[string]string{
		"client_id":             e.hca.clientID,
		"redirect_uri":          testBaseURL + "/auth/callback",
		"response_type":         "code",
		"scope":                 "email name",
		"code_challenge_method": "S256",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if len(ap.state) < 32 || len(ap.challenge) != 43 {
		t.Errorf("state %q / challenge %q look too weak", ap.state, ap.challenge)
	}
	c := ap.cookie
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/auth/" || c.MaxAge <= 0 || c.MaxAge > 600 {
		t.Errorf("state cookie attributes = %+v", c)
	}
	if strings.Contains(c.Value, ap.challenge) {
		t.Error("state cookie should hold the verifier, not the challenge")
	}
}

func TestLoginWithoutHCAConfigIsUnavailable(t *testing.T) {
	e := newEnv(t, func(o *auth.Options) { o.HCA.ClientID = "" })
	if rec := e.do(req{target: "/auth/login"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestCallbackSignsInAndCreatesUser(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)

	if b.name != "fiona-hackworth" {
		t.Errorf("username = %q, want fiona-hackworth", b.name)
	}
	c := b.cookie
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("session cookie attributes = %+v", c)
	}
	// Only the hash of the cookie is stored.
	if n := e.count("SELECT count(*) FROM sessions WHERE id_hash = $1", []byte(c.Value)); n != 0 {
		t.Error("raw session id stored in the database")
	}
	if n := e.count("SELECT count(*) FROM users WHERE hca_id = $1 AND email = 'fiona@example.com' AND display_name = 'Fiona Hackworth'", fiona.ID); n != 1 {
		t.Errorf("user rows = %d, want 1", n)
	}
	if n := e.count("SELECT count(*) FROM audit_log WHERE action = 'auth.login' AND data->>'method' = 'hca'"); n != 1 {
		t.Errorf("auth.login audit rows = %d, want 1", n)
	}

	// Signing in again keeps the same account and username.
	again := e.login(fiona)
	if again.userID != b.userID || again.name != b.name {
		t.Errorf("second sign-in got %s/%s, want %s/%s", again.userID, again.name, b.userID, b.name)
	}
	// A different person with the same name gets a suffixed username.
	other := e.login(fakeIdentity{ID: "ident!fiona2", FirstName: "Fiona", LastName: "Hackworth"})
	if other.name != "fiona-hackworth2" || other.userID == b.userID {
		t.Errorf("namesake got %s/%s", other.userID, other.name)
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name   string
		mutate func(ap *authorizeParams, q url.Values) []*http.Cookie
	}{
		{"state mismatch", func(ap *authorizeParams, q url.Values) []*http.Cookie {
			q.Set("state", ap.state+"x")
			return []*http.Cookie{ap.cookie}
		}},
		{"missing state", func(ap *authorizeParams, q url.Values) []*http.Cookie {
			q.Del("state")
			return []*http.Cookie{ap.cookie}
		}},
		{"no state cookie", func(_ *authorizeParams, _ url.Values) []*http.Cookie { return nil }},
		{"tampered cookie", func(ap *authorizeParams, _ url.Values) []*http.Cookie {
			c := *ap.cookie
			c.Value = "A" + c.Value[1:]
			return []*http.Cookie{&c}
		}},
		{"cookie from another login", func(_ *authorizeParams, _ url.Values) []*http.Cookie {
			return []*http.Cookie{e.startLogin("").cookie}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ap := e.startLogin("")
			q := url.Values{"code": {e.hca.issueCode(ap.challenge, fiona)}, "state": {ap.state}}
			cookies := tc.mutate(&ap, q)
			rec := e.do(req{target: "/auth/callback?" + q.Encode(), cookies: cookies})
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/?auth_error=invalid_state" {
				t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
			}
			if cookieNamed(rec, auth.SessionCookie) != nil {
				t.Fatal("session cookie set despite bad state")
			}
		})
	}
	if n := e.count("SELECT count(*) FROM users"); n != 0 {
		t.Errorf("users created = %d, want 0", n)
	}
}

func TestCallbackStateExpires(t *testing.T) {
	e := newEnv(t)
	ap := e.startLogin("")
	e.clock.Advance(11 * 60e9) // past the 10-minute state lifetime
	code := e.hca.issueCode(ap.challenge, fiona)
	rec := e.do(req{target: "/auth/callback?code=" + code + "&state=" + url.QueryEscape(ap.state), cookies: []*http.Cookie{ap.cookie}})
	if got := rec.Header().Get("Location"); got != "/?auth_error=invalid_state" {
		t.Fatalf("Location = %q", got)
	}
}

func TestCallbackEnforcesPKCE(t *testing.T) {
	e := newEnv(t)
	ap := e.startLogin("")
	// A code bound to someone else's challenge (an injected code) must fail
	// at the token endpoint because our verifier doesn't match it.
	other := sha256.Sum256([]byte("attacker verifier"))
	code := e.hca.issueCode(base64.RawURLEncoding.EncodeToString(other[:]), fiona)
	rec := e.do(req{target: "/auth/callback?code=" + code + "&state=" + url.QueryEscape(ap.state), cookies: []*http.Cookie{ap.cookie}})
	if got := rec.Header().Get("Location"); got != "/?auth_error=failed" {
		t.Fatalf("Location = %q, want failure", got)
	}
	if cookieNamed(rec, auth.SessionCookie) != nil {
		t.Fatal("session created without a valid PKCE verifier")
	}
}

func TestCallbackProviderError(t *testing.T) {
	e := newEnv(t)
	ap := e.startLogin("")
	rec := e.do(req{target: "/auth/callback?error=access_denied&state=" + url.QueryEscape(ap.state), cookies: []*http.Cookie{ap.cookie}})
	if got := rec.Header().Get("Location"); got != "/?auth_error=access_denied" {
		t.Fatalf("Location = %q", got)
	}
}

func TestCallbackStateIsSingleUse(t *testing.T) {
	e := newEnv(t)
	ap := e.startLogin("")
	code := e.hca.issueCode(ap.challenge, fiona)
	rec := e.do(req{target: "/auth/callback?code=" + code + "&state=" + url.QueryEscape(ap.state), cookies: []*http.Cookie{ap.cookie}})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("first callback = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if c := cookieNamed(rec, "logbook_oauth"); c == nil || c.MaxAge >= 0 {
		t.Fatalf("state cookie not cleared: %+v", c)
	}
}

func TestCallbackNextRedirect(t *testing.T) {
	tests := []struct{ next, want string }{
		{"/projects?range=week", "/projects?range=week"},
		{"", "/"},
		{"//evil.example/x", "/"},
		{"/\\evil.example", "/"},
		{"https://evil.example/", "/"},
		{"javascript:alert(1)", "/"},
		{"/\r\nSet-Cookie:x=y", "/"},
		{"/%2F%2Fevil.example", "/%2F%2Fevil.example"},
	}
	e := newEnv(t)
	for _, tc := range tests {
		t.Run(tc.next, func(t *testing.T) {
			ap := e.startLogin(tc.next)
			code := e.hca.issueCode(ap.challenge, fiona)
			rec := e.do(req{target: "/auth/callback?code=" + code + "&state=" + url.QueryEscape(ap.state), cookies: []*http.Cookie{ap.cookie}})
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoginReplacesExistingSession(t *testing.T) {
	e := newEnv(t)
	old := e.login(fiona)
	ap := e.startLogin("")
	code := e.hca.issueCode(ap.challenge, fiona)
	rec := e.do(req{
		target:  "/auth/callback?code=" + code + "&state=" + url.QueryEscape(ap.state),
		cookies: []*http.Cookie{ap.cookie, old.cookie},
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d", rec.Code)
	}
	if rec := old.get(e, "/api/v1/session"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old session still valid: %d", rec.Code)
	}
}

func TestAuthRoutesAreRateLimitedPerIP(t *testing.T) {
	e := newEnv(t)
	limited := false
	for range 30 {
		if rec := e.do(req{target: "/auth/login", remoteAddr: "198.51.100.7:5000"}); rec.Code == http.StatusTooManyRequests {
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("30 rapid logins from one IP were never limited")
	}
	// Another IP is unaffected.
	if rec := e.do(req{target: "/auth/login", remoteAddr: "198.51.100.8:5000"}); rec.Code != http.StatusFound {
		t.Fatalf("other IP status = %d", rec.Code)
	}
}
