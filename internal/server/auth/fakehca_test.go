package auth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeHCA is a minimal Hack Club Auth: a token endpoint that enforces PKCE
// (S256) and client credentials, and /api/v1/me.
type fakeHCA struct {
	*httptest.Server
	clientID, clientSecret, redirectURI string

	mu     sync.Mutex
	codes  map[string]fakeGrant // code -> grant
	tokens map[string]fakeIdentity
	n      int
}

type fakeGrant struct {
	challenge string
	identity  fakeIdentity
}

type fakeIdentity struct {
	ID        string `json:"id"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Email     string `json:"primary_email,omitempty"`
}

func newFakeHCA(t *testing.T, redirectURI string) *fakeHCA {
	t.Helper()
	f := &fakeHCA{
		clientID: "client-id", clientSecret: "client-secret", redirectURI: redirectURI,
		codes: map[string]fakeGrant{}, tokens: map[string]fakeIdentity{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", f.token)
	mux.HandleFunc("GET /api/v1/me", f.me)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// issueCode simulates the user approving our app: it binds a fresh code to
// the PKCE challenge from the authorize URL.
func (f *fakeHCA) issueCode(challenge string, id fakeIdentity) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	code := fmt.Sprintf("code-%d", f.n)
	f.codes[code] = fakeGrant{challenge: challenge, identity: id}
	return code
}

func oauthErr(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (f *fakeHCA) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthErr(w, "invalid_request")
		return
	}
	if r.PostForm.Get("client_id") != f.clientID || r.PostForm.Get("client_secret") != f.clientSecret {
		oauthErr(w, "invalid_client")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != f.redirectURI {
		oauthErr(w, "invalid_request")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code := r.PostForm.Get("code")
	g, ok := f.codes[code]
	if !ok {
		oauthErr(w, "invalid_grant")
		return
	}
	delete(f.codes, code) // codes are single use
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		oauthErr(w, "invalid_grant")
		return
	}
	tok := "idntk." + code
	f.tokens[tok] = g.identity
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 3600})
}

func (f *fakeHCA) me(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	id, ok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"identity": id, "scopes": []string{"email", "name"}})
}
