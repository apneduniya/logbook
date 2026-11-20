package auth_test

import (
	"net/http"
	"testing"

	"github.com/apneduniya/logbook/internal/server/auth"
)

func devEnv(t *testing.T, enabled bool, baseURL string) *env {
	t.Helper()
	return newEnv(t, func(o *auth.Options) {
		o.DevLogin = enabled
		o.BaseURL = baseURL
		o.InsecureCookies = true
	})
}

func TestDevLoginRefusedWhenNotEnabled(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		baseURL string
	}{
		{"disabled on localhost", false, "http://127.0.0.1:8080"},
		{"enabled but public host", true, "https://logbook.example.com"},
		{"enabled but lookalike host", true, "http://127.0.0.1.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := devEnv(t, tc.enabled, tc.baseURL)
			rec := e.do(req{method: http.MethodPost, target: "/auth/dev-login", body: `{"username":"orpheus"}`})
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if cookieNamed(rec, auth.SessionCookie) != nil {
				t.Fatal("session cookie issued")
			}
			if n := e.count("SELECT count(*) FROM users"); n != 0 {
				t.Fatalf("users created = %d", n)
			}
		})
	}
}

func TestDevLogin(t *testing.T) {
	for _, base := range []string{"http://127.0.0.1:8080", "http://localhost:5173"} {
		t.Run(base, func(t *testing.T) {
			e := devEnv(t, true, base)
			rec := e.do(req{method: http.MethodPost, target: "/auth/dev-login", body: `{"username":"Orpheus"}`})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			c := cookieNamed(rec, auth.SessionCookie)
			if c == nil || c.Secure {
				t.Fatalf("dev cookie = %+v", c)
			}
			b := e.sessionFor(c)
			if b.name != "orpheus" {
				t.Fatalf("username = %q", b.name)
			}
			if n := e.count("SELECT count(*) FROM users WHERE hca_id = 'dev:orpheus'"); n != 1 {
				t.Fatalf("dev users = %d", n)
			}
			// Same name returns the same user.
			rec = e.do(req{method: http.MethodPost, target: "/auth/dev-login", body: `{"username":"orpheus"}`})
			if again := e.sessionFor(cookieNamed(rec, auth.SessionCookie)); again.userID != b.userID {
				t.Fatal("dev login created a second user")
			}
		})
	}
}

func TestDevLoginValidation(t *testing.T) {
	e := devEnv(t, true, "http://127.0.0.1:8080")
	tests := []struct {
		name, body, contentType string
		want                    int
	}{
		{"form post", "username=x", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"text plain", `{"username":"orpheus"}`, "text/plain", http.StatusUnsupportedMediaType},
		{"too short", `{"username":"o"}`, "application/json", http.StatusBadRequest},
		{"bad chars", `{"username":"or pheus"}`, "application/json", http.StatusBadRequest},
		{"bad json", `{`, "application/json", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(req{method: http.MethodPost, target: "/auth/dev-login", body: tc.body,
				header: map[string]string{"Content-Type": tc.contentType}})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
