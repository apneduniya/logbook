package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSafeNext(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/dashboard", "/dashboard"},
		{"/p/logbook?range=week#x", "/p/logbook?range=week"},
		{"//evil.example", "/"},
		{"///evil.example", "/"},
		{"/\\evil.example", "/"},
		{"\\\\evil.example", "/"},
		{"https://evil.example", "/"},
		{"http:/evil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"evil.example", "/"},
		{"/\tevil", "/"},
		{"/a\r\nLocation: x", "/"},
		{"/" + strings.Repeat("a", maxNextLen), "/"},
		{"/%2F%2Fevil.example", "/%2F%2Fevil.example"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := safeNext(tc.in); got != tc.want {
				t.Fatalf("safeNext(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeUsername(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Fiona Hackworth", "fiona-hackworth"},
		{"  Zoë  O'Brien ", "zo-obrien"},
		{"a", ""},
		{"李明", ""},
		{"__x__", ""},
		{"john.doe+tag", "john-doe-tag"},
		{"--a--b--", "a-b"},
		{strings.Repeat("abc", 20), strings.Repeat("abc", 20)[:maxBaseUsername]},
		{"under_score", "under_score"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := sanitizeUsername(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeUsername(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got != "" && !usernameRe.MatchString(got) {
				t.Fatalf("%q violates the users.username check", got)
			}
		})
	}
}

func TestBaseUsername(t *testing.T) {
	tests := []struct {
		id   hcaIdentity
		want string
	}{
		{hcaIdentity{FirstName: "Fiona", LastName: "Hackworth"}, "fiona-hackworth"},
		// Email is private: never a source for the public username.
		{hcaIdentity{PrimaryEmail: "orpheus.dino@hackclub.com"}, fallbackUsername},
		{hcaIdentity{FirstName: "李", PrimaryEmail: "jane.doe2009@gmail.com"}, fallbackUsername},
		{hcaIdentity{FirstName: "देव", LastName: "शर्मा", PrimaryEmail: "dev@example.com"}, fallbackUsername},
		{hcaIdentity{}, fallbackUsername},
	}
	for _, tc := range tests {
		if got := baseUsername(tc.id); got != tc.want {
			t.Errorf("baseUsername(%+v) = %q, want %q", tc.id, got, tc.want)
		}
	}
	if got := usernameCandidate("fiona", 0); got != "fiona" {
		t.Errorf("candidate 0 = %q", got)
	}
	if got := usernameCandidate("fiona", 1); got != "fiona2" {
		t.Errorf("candidate 1 = %q", got)
	}
	long := strings.Repeat("a", maxBaseUsername)
	if got := usernameCandidate(long, 999_998); !usernameRe.MatchString(got) {
		t.Errorf("suffixed max-length username %q is invalid", got)
	}
}

func TestAPIKeyFromRequest(t *testing.T) {
	const key = "3f1e2d4c-1111-4222-8333-444455556666"
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		name, auth, query, want string
		ok                      bool
	}{
		{"bearer", "Bearer " + key, "", key, true},
		{"bearer lowercase scheme", "bearer " + key, "", key, true},
		{"basic key", "Basic " + b64(key), "", key, true},
		{"basic key colon", "Basic " + b64(key+":"), "", key, true},
		{"basic colon key", "Basic " + b64(":"+key), "", key, true},
		{"basic empty", "Basic " + b64(":"), "", "", false},
		{"query", "", key, key, true},
		{"header wins over query", "Bearer " + key, "other", key, true},
		{"malformed header", "Bearer", "", "", false},
		{"unknown scheme", "Digest " + key, key, "", false},
		{"nothing", "", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x?api_key="+tc.query, nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			got, ok := apiKeyFromRequest(r)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestStateCookieMAC(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	mk := func(secret string) *Service {
		s, err := New(nil, Options{BaseURL: "https://logbook.test", SecretKey: []byte(strings.Repeat(secret, 32)),
			Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := mk("a"), mk("b")
	v, err := a.encodeState(oauthState{State: "s", Verifier: "v", Next: "/", Expires: now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.decodeState(v); err != nil {
		t.Fatalf("own cookie rejected: %v", err)
	}
	if _, err := b.decodeState(v); err == nil {
		t.Fatal("cookie MAC'd with another key accepted")
	}
	payload, sig, _ := strings.Cut(v, ".")
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"s":"x","v":"v","n":"//evil","e":9999999999}`))
	for _, bad := range []string{forged + "." + sig, payload, payload + ".", "." + sig, ""} {
		if _, err := a.decodeState(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	expired, _ := a.encodeState(oauthState{State: "s", Verifier: "v", Expires: now.Add(-time.Second).Unix()})
	if _, err := a.decodeState(expired); err == nil {
		t.Fatal("expired state accepted")
	}
}

func TestCleanupInterval(t *testing.T) {
	tests := []struct{ in, want time.Duration }{
		{0, DefaultSessionCleanupInterval},
		{-time.Second, DefaultSessionCleanupInterval},
		{15 * time.Minute, 15 * time.Minute},
	}
	for _, tc := range tests {
		if got := cleanupInterval(tc.in); got != tc.want {
			t.Errorf("cleanupInterval(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsLoopbackPeer(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:5000", true},
		{"127.8.9.10:5000", true},
		{"[::1]:5000", true},
		{"192.168.1.20:5000", false},
		{"[2001:db8::1]:5000", false},
		{"localhost:5000", false}, // RemoteAddr is always an IP
		{"127.0.0.1", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isLoopbackPeer(tc.addr); got != tc.want {
			t.Errorf("isLoopbackPeer(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestNewRejectsWeakConfig(t *testing.T) {
	if _, err := New(nil, Options{BaseURL: "https://x.test", SecretKey: []byte("short")}); err == nil {
		t.Error("short secret accepted")
	}
	if _, err := New(nil, Options{BaseURL: "/relative", SecretKey: make([]byte, 32)}); err == nil {
		t.Error("relative base URL accepted")
	}
}
