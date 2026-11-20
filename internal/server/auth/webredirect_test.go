package auth_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/server/auth"
)

const testWebURL = "https://web.logbook.test"

func webEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, func(o *auth.Options) { o.WebURL = testWebURL + "/" })
}

// With the UI on its own origin (ADR 0011), sign-in lands on the web origin
// plus the saved next path, and nothing a user controls can change the host.
func TestCallbackRedirectsToWebURL(t *testing.T) {
	tests := []struct{ next, want string }{
		{"", testWebURL + "/"},
		{"/projects?range=week", testWebURL + "/projects?range=week"},
		{"/@evil.example", testWebURL + "/@evil.example"},
		{"//evil.example/x", testWebURL + "/"},
		{"///evil.example", testWebURL + "/"},
		{"/\\evil.example", testWebURL + "/"},
		{"https://evil.example/", testWebURL + "/"},
		{"http:evil.example", testWebURL + "/"},
		{"javascript:alert(1)", testWebURL + "/"},
		{".evil.example", testWebURL + "/"},
		{"/\r\nLocation: https://evil.example", testWebURL + "/"},
		{"/%2F%2Fevil.example", testWebURL + "/%2F%2Fevil.example"},
	}
	e := webEnv(t)
	for _, tc := range tests {
		t.Run(tc.next, func(t *testing.T) {
			ap := e.startLogin(tc.next)
			code := e.hca.issueCode(ap.challenge, fiona)
			rec := e.do(req{target: "/auth/callback?code=" + code + "&state=" + url.QueryEscape(ap.state), cookies: []*http.Cookie{ap.cookie}})
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d", rec.Code)
			}
			got := rec.Header().Get("Location")
			if got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil || u.Scheme+"://"+u.Host != testWebURL {
				t.Fatalf("Location %q leaves the web origin", got)
			}
		})
	}
}

func TestCallbackFailureRedirectsToWebURL(t *testing.T) {
	e := webEnv(t)
	ap := e.startLogin("")
	rec := e.do(req{target: "/auth/callback?error=access_denied&state=" + url.QueryEscape(ap.state), cookies: []*http.Cookie{ap.cookie}})
	if got, want := rec.Header().Get("Location"), testWebURL+"/?auth_error=access_denied"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestNewRejectsWebURLThatIsNotAnOrigin(t *testing.T) {
	for _, web := range []string{"https://web.test/app", "https://web.test/?x", "https://u@web.test", "web.test", "ftp://web.test"} {
		t.Run(web, func(t *testing.T) {
			_, err := auth.New(nil, auth.Options{BaseURL: testBaseURL, WebURL: web, SecretKey: []byte(strings.Repeat("k", 32))})
			if err == nil {
				t.Fatalf("New accepted web URL %q", web)
			}
		})
	}
}

func devWebEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, func(o *auth.Options) {
		o.DevLogin = true
		o.BaseURL = "http://127.0.0.1:8080"
		o.WebURL = "http://localhost:5173"
		o.InsecureCookies = true
	})
}

// The web UI signs in to dev login with a form post and is sent back to its
// own origin.
func TestDevLoginFormRedirectsToWebURL(t *testing.T) {
	tests := []struct{ next, want string }{
		{"", "http://localhost:5173/"},
		{"/projects", "http://localhost:5173/projects"},
		{"//evil.example", "http://localhost:5173/"},
		{"https://evil.example", "http://localhost:5173/"},
		{"/\\evil.example", "http://localhost:5173/"},
	}
	e := devWebEnv(t)
	for _, tc := range tests {
		t.Run(tc.next, func(t *testing.T) {
			body := url.Values{"username": {"orpheus"}, "next": {tc.next}}.Encode()
			rec := e.do(req{method: http.MethodPost, target: "/auth/dev-login", remoteAddr: devPeer, body: body,
				header: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://localhost:5173"}})
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
			if cookieNamed(rec, auth.SessionCookie) == nil {
				t.Fatal("no session cookie")
			}
		})
	}
}

// A form on any other page can't sign the browser in (login CSRF).
func TestDevLoginFormRejectsOtherOrigins(t *testing.T) {
	e := devWebEnv(t)
	for _, origin := range []string{"", "null", "http://evil.example", "http://localhost:5174", "https://localhost:5173"} {
		t.Run(origin, func(t *testing.T) {
			h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
			if origin != "" {
				h["Origin"] = origin
			}
			rec := e.do(req{method: http.MethodPost, target: "/auth/dev-login", remoteAddr: devPeer, body: "username=orpheus", header: h})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if cookieNamed(rec, auth.SessionCookie) != nil {
				t.Fatal("session cookie issued")
			}
		})
	}
	if n := e.count("SELECT count(*) FROM users"); n != 0 {
		t.Fatalf("users created = %d", n)
	}
}
