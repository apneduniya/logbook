package oauth

import (
	"strings"
	"testing"
)

func TestParseScopes(t *testing.T) {
	tests := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"", "profile", true},
		{"profile", "profile", true},
		{"read profile", "profile read", true},
		{"read  read", "read", true},
		{"profile read admin", "", false},
		{"submissions", "", false},
		{"PROFILE", "", false},
	}
	for _, tc := range tests {
		got, ok := parseScopes(tc.raw, userScopes, ScopeProfile)
		if ok != tc.ok || scopeString(got) != tc.want {
			t.Errorf("parseScopes(%q) = %q %v, want %q %v", tc.raw, scopeString(got), ok, tc.want, tc.ok)
		}
	}
}

func TestCheckRedirectURI(t *testing.T) {
	tests := []struct {
		uri string
		ok  bool
	}{
		{"https://program.test/callback", true},
		{"https://program.test:8443/cb?x=1", true},
		{"http://localhost/cb", true},
		{"http://localhost:3000/cb", true},
		{"http://127.0.0.1:8080/cb", true},
		{"http://program.test/cb", false},
		{"http://localhost.evil.test/cb", false},
		{"https://program.test/cb#frag", false},
		{"https://user:pw@program.test/cb", false},
		{"https:///cb", false},
		{"/cb", false},
		{"myapp://cb", false},
		{"javascript:alert(1)", false},
		{"https://program.test/c b", false},
		{"https://program.test/" + strings.Repeat("a", MaxURLLen), false},
	}
	for _, tc := range tests {
		if err := checkRedirectURI(tc.uri); (err == nil) != tc.ok {
			t.Errorf("checkRedirectURI(%q) = %v, want ok=%v", tc.uri, err, tc.ok)
		}
	}
}

func TestVerifyPKCE(t *testing.T) {
	// RFC 7636 Appendix B.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if !verifyPKCE(challenge, verifier) {
		t.Fatal("RFC 7636 example does not verify")
	}
	if verifyPKCE(challenge, verifier[:42]+"Y") {
		t.Fatal("wrong verifier verifies")
	}
	if verifyPKCE(challenge, "short") {
		t.Fatal("short verifier verifies")
	}
	if !validChallenge(challenge) || validChallenge("abc") || validChallenge(strings.Repeat("a", 42)+"!") {
		t.Fatal("validChallenge")
	}
}

func TestWellFormed(t *testing.T) {
	tok := newSecret(accessPrefix)
	if !wellFormed(tok, accessPrefix) || wellFormed(tok, refreshPrefix) || wellFormed(tok+"A", accessPrefix) ||
		wellFormed("lbo_!!!", accessPrefix) {
		t.Fatal("wellFormed")
	}
	if len(tok) != len(accessPrefix)+43 {
		t.Fatalf("token length %d", len(tok))
	}
}

func TestWithQuery(t *testing.T) {
	v := map[string][]string{"code": {"c"}}
	for in, want := range map[string]string{
		"https://a.test/cb":      "https://a.test/cb?code=c",
		"https://a.test/cb?x=1":  "https://a.test/cb?x=1&code=c",
		"https://a.test/cb?":     "https://a.test/cb?code=c",
		"https://a.test/cb?x=1&": "https://a.test/cb?x=1&code=c",
	} {
		if got := withQuery(in, v); got != want {
			t.Errorf("withQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
