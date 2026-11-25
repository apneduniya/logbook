package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

const testOrigin = "https://logbook.boogles.dev"

func corsHandler() http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // proves the request reached the handler
	})
	return CORS(CORSConfig{Origin: testOrigin, Skip: []string{"/api/v1/users/current/heartbeats", "/api/v1/devices"}})(next)
}

func corsDo(t *testing.T, method, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	corsHandler().ServeHTTP(rec, r)
	return rec
}

func TestCORSPreflight(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		method  string
		headers string
		want    int
	}{
		{"allowed GET", testOrigin, "GET", "", http.StatusNoContent},
		{"allowed PATCH with CSRF header", testOrigin, "PATCH", "Content-Type, X-CSRF-Token", http.StatusNoContent},
		{"allowed DELETE, header case", testOrigin, "DELETE", "x-csrf-token", http.StatusNoContent},
		{"other origin", "https://evil.example", "GET", "", http.StatusForbidden},
		{"lookalike origin", "https://logbook.boogles.dev.evil.example", "GET", "", http.StatusForbidden},
		{"origin with path", testOrigin + "/", "GET", "", http.StatusForbidden},
		{"http scheme", "http://logbook.boogles.dev", "GET", "", http.StatusForbidden},
		{"null origin", "null", "GET", "", http.StatusForbidden},
		{"method not allowed", testOrigin, "PUT", "", http.StatusForbidden},
		{"header not allowed", testOrigin, "POST", "Content-Type, Authorization", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]string{"Origin": tc.origin, "Access-Control-Request-Method": tc.method}
			if tc.headers != "" {
				h["Access-Control-Request-Headers"] = tc.headers
			}
			rec := corsDo(t, http.MethodOptions, "/api/v1/me/projects/x", h)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			got := rec.Header()
			if !slices.Contains(got.Values("Vary"), "Origin") {
				t.Errorf("Vary = %q, want Origin", got.Values("Vary"))
			}
			if tc.want != http.StatusNoContent {
				if got.Get("Access-Control-Allow-Origin") != "" || got.Get("Access-Control-Allow-Credentials") != "" {
					t.Errorf("rejected preflight carries allow headers: %v", got)
				}
				return
			}
			checks := map[string]string{
				"Access-Control-Allow-Origin":      testOrigin,
				"Access-Control-Allow-Credentials": "true",
				"Access-Control-Allow-Methods":     "GET, POST, PATCH, DELETE",
				"Access-Control-Allow-Headers":     "Content-Type, X-CSRF-Token",
				"Access-Control-Max-Age":           "600",
			}
			for k, want := range checks {
				if v := got.Get(k); v != want {
					t.Errorf("%s = %q, want %q", k, v, want)
				}
			}
		})
	}
}

func TestCORSActualRequest(t *testing.T) {
	tests := []struct {
		name, method, path, origin string
		wantAllow                  bool
	}{
		{"allowed origin GET", http.MethodGet, "/api/v1/me/summary", testOrigin, true},
		{"allowed origin DELETE", http.MethodDelete, "/api/v1/me/devices/1", testOrigin, true},
		{"public profile", http.MethodGet, "/api/v1/users/current/profile", testOrigin, true},
		{"other origin", http.MethodGet, "/api/v1/me/summary", "https://evil.example", false},
		{"no origin (same-origin or non-browser)", http.MethodGet, "/api/v1/me/summary", "", false},
		{"auth routes never get CORS", http.MethodGet, "/auth/login", testOrigin, false},
		{"health check", http.MethodGet, "/healthz", testOrigin, false},
		{"heartbeat ingest is skipped", http.MethodPost, "/api/v1/users/current/heartbeats.bulk", testOrigin, false},
		{"agent uploads are skipped", http.MethodPost, "/api/v1/devices/1/minutes", testOrigin, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]string{}
			if tc.origin != "" {
				h["Origin"] = tc.origin
			}
			rec := corsDo(t, tc.method, tc.path, h)
			// Disallowed origins still reach the handler; the browser, not the
			// server, withholds the response from the page.
			if rec.Code != http.StatusTeapot {
				t.Fatalf("status = %d, want the handler's 418", rec.Code)
			}
			allow := rec.Header().Get("Access-Control-Allow-Origin")
			creds := rec.Header().Get("Access-Control-Allow-Credentials")
			if tc.wantAllow {
				if allow != testOrigin || creds != "true" {
					t.Fatalf("allow origin %q, credentials %q", allow, creds)
				}
				return
			}
			if allow != "" || creds != "" {
				t.Fatalf("unexpected CORS headers: origin %q, credentials %q", allow, creds)
			}
		})
	}
}

func TestCORSSkippedPreflightFallsThrough(t *testing.T) {
	rec := corsDo(t, http.MethodOptions, "/auth/dev-login", map[string]string{
		"Origin": testOrigin, "Access-Control-Request-Method": "POST",
	})
	if rec.Code != http.StatusTeapot || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("status = %d, allow origin %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCORSWithoutOriginAllowsNothing(t *testing.T) {
	h := CORS(CORSConfig{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/api/v1/me/summary", nil)
	r.Header.Set("Origin", "")
	r.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSkipMatches(t *testing.T) {
	tests := []struct {
		path, prefix string
		want         bool
	}{
		{"/api/v1/devices/abc/minutes", "/api/v1/devices", true},
		{"/api/v1/devicesx", "/api/v1/devices", true}, // plain entries are string prefixes
		{"/api/v1/users/orpheus/stats", "/api/v1/users/{id}/stats", true},
		{"/api/v1/users/42/heartbeats/spans", "/api/v1/users/{id}/heartbeats", true},
		{"/api/v1/users/42/projects/details", "/api/v1/users/{id}/projects", true},
		{"/api/v1/users/orpheus/profile", "/api/v1/users/{id}/stats", false},
		{"/api/v1/users/orpheus/live", "/api/v1/users/{id}/project", false},
		{"/api/v1/users/orpheus/projects", "/api/v1/users/{id}/project", false}, // whole segments
		{"/api/v1/users//stats", "/api/v1/users/{id}/stats", false},
		{"/api/v1/users/orpheus", "/api/v1/users/{id}/stats", false},
	}
	for _, tc := range tests {
		if got := skipMatches(tc.path, tc.prefix); got != tc.want {
			t.Errorf("skipMatches(%q, %q) = %v, want %v", tc.path, tc.prefix, got, tc.want)
		}
	}
}
