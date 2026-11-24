package oauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/auth"
	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/oauth"
)

func TestRevokedGrantKillsTokens(t *testing.T) {
	e := newEnv(t)
	owner, alice, bob := e.user("owner"), e.user("alice"), e.user("bob")
	app := e.createApp(owner, "stardance")
	other := e.createApp(owner, "other")
	tok := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile read")), "")
	pending := e.codeFor(alice, authParams(app, "profile"))
	otherTok := e.mustExchange(other, e.codeFor(alice, authParams(other, "profile")), "")
	bobTok := e.mustExchange(app, e.codeFor(bob, authParams(app, "profile")), "")

	gs := decode[map[string][]oauth.Grant](t, e.do(http.MethodGet, "/api/v1/me/grants", alice, ""))["grants"]
	if len(gs) != 2 {
		t.Fatalf("alice's grants = %+v", gs)
	}
	// Bob sees only his own grant and can't revoke alice's.
	bobs := decode[map[string][]oauth.Grant](t, e.do(http.MethodGet, "/api/v1/me/grants", bob, ""))["grants"]
	if len(bobs) != 1 || bobs[0].AppID.String() != app.ID {
		t.Fatalf("bob's grants = %+v", bobs)
	}
	if rec := e.do(http.MethodDelete, "/api/v1/me/grants/"+other.ID, bob, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob revokes a grant he doesn't have = %d", rec.Code)
	}
	if rec := e.probe("/probe/profile", otherTok.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("alice's token after bob's attempt = %d", rec.Code)
	}

	if rec := e.do(http.MethodDelete, "/api/v1/me/grants/"+app.ID, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if rec := e.probe("/probe/profile", tok.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Errorf("token after revoke = %d", rec.Code)
	}
	expectOAuthError(t, e.post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}, creds(app)),
		http.StatusBadRequest, "invalid_grant")
	expectOAuthError(t, e.exchange(app, pending, ""), http.StatusBadRequest, "invalid_grant")
	if rec := e.probe("/probe/profile", otherTok.AccessToken); rec.Code != http.StatusOK {
		t.Errorf("another app's token = %d", rec.Code)
	}
	if rec := e.probe("/probe/profile", bobTok.AccessToken); rec.Code != http.StatusOK {
		t.Errorf("another user's token = %d", rec.Code)
	}
	if rec := e.do(http.MethodDelete, "/api/v1/me/grants/"+app.ID, alice, ""); rec.Code != http.StatusNotFound {
		t.Errorf("revoke twice = %d", rec.Code)
	}
	// After revoking, the next authorize asks again.
	consentID(t, e.authorize(alice, authParams(app, "profile")))
	if got := actions(t, e.st, alice); got[len(got)-1] != "oauth.revoke" {
		t.Errorf("audit = %v", got)
	}
}

func TestRevokeEndpoint(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	other := e.createApp(owner, "other")
	tok := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile")), "")

	// Another client's revoke succeeds on the wire but changes nothing.
	if rec := e.post("/oauth/revoke", url.Values{"token": {tok.AccessToken}}, creds(other)); rec.Code != http.StatusOK {
		t.Fatalf("other client revoke = %d", rec.Code)
	}
	if rec := e.probe("/probe/profile", tok.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("token after another client's revoke = %d", rec.Code)
	}
	// Bad client credentials are refused.
	expectOAuthError(t, e.post("/oauth/revoke", url.Values{"token": {tok.AccessToken}}, &[2]string{app.ClientID, "wrong"}),
		http.StatusUnauthorized, "invalid_client")
	// Unknown tokens are fine.
	for _, junk := range []string{"", "nope", "lbo_" + strings.Repeat("A", 43)} {
		if rec := e.post("/oauth/revoke", url.Values{"token": {junk}}, creds(app)); rec.Code != http.StatusOK || rec.Body.String() != "{}\n" {
			t.Errorf("revoke %q = %d %q", junk, rec.Code, rec.Body)
		}
	}
	// Revoking the refresh token kills the access token too.
	if rec := e.post("/oauth/revoke", url.Values{"token": {tok.RefreshToken}, "token_type_hint": {"refresh_token"}}, creds(app)); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if rec := e.probe("/probe/profile", tok.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token after revoke = %d", rec.Code)
	}
	if got := actions(t, e.st, alice); got[len(got)-1] != "oauth.revoke" {
		t.Errorf("audit = %v", got)
	}
}

func TestBearerMiddleware(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	tok := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile read")), "")

	tests := []struct {
		name, header string
		want         int
	}{
		{"none", "", http.StatusUnauthorized},
		{"bearer", "Bearer " + tok.AccessToken, http.StatusOK},
		{"lower-case scheme", "bearer " + tok.AccessToken, http.StatusOK},
		{"refresh token", "Bearer " + tok.RefreshToken, http.StatusUnauthorized},
		{"client secret", "Bearer " + app.ClientSecret, http.StatusUnauthorized},
		{"basic", "Basic " + tok.AccessToken, http.StatusUnauthorized},
		{"junk", "Bearer lbo_junk", http.StatusUnauthorized},
		{"api key shape", "Bearer 3f1e2d4c-1111-4222-8333-444455556666", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/probe/read", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, rec.Code, tc.want)
		}
		if tc.want == http.StatusUnauthorized && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: WWW-Authenticate = %q", tc.name, rec.Header().Get("WWW-Authenticate"))
		}
	}
	// A session never satisfies RequireScope.
	if rec := e.do(http.MethodGet, "/probe/profile", alice, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session on bearer route = %d", rec.Code)
	}

	// OptionalScope: a good token is used; anything else is anonymous.
	if got := decode[probeJSON](t, e.probe("/probe/optional", tok.AccessToken)); got.User != alice.String() {
		t.Fatalf("optional with token = %+v", got)
	}
	if got := decode[probeJSON](t, e.probe("/probe/optional", "lbo_junk")); got.User != "" {
		t.Fatalf("optional with junk = %+v", got)
	}
	profileOnly := e.mustExchange(app, e.codeFor(e.user("bob"), authParams(app, "profile")), "")
	if got := decode[probeJSON](t, e.probe("/probe/optional", profileOnly.AccessToken)); got.User != "" {
		t.Fatalf("optional with under-scoped token = %+v", got)
	}
}

// TestCredentialsDontCross pins that OAuth access tokens and user API keys
// each work only where they belong, using the real auth middleware.
func TestCredentialsDontCross(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	tok := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile read")), "")

	authSvc, err := auth.New(e.st, auth.Options{BaseURL: "http://127.0.0.1:8080", SecretKey: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	key, err := authSvc.CreateAPIKey(context.Background(), alice, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /key", authSvc.RequireAPIKey(ok))
	mux.Handle("GET /session", authSvc.RequireSession(ok))
	mux.Handle("GET /scope", e.svc.RequireScope(oauth.ScopeRead)(ok))
	call := func(path, bearer string) int {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec.Code
	}
	if got := call("/key", key.Key); got != http.StatusOK {
		t.Fatalf("API key on API-key route = %d", got)
	}
	if got := call("/scope", tok.AccessToken); got != http.StatusOK {
		t.Fatalf("OAuth token on scope route = %d", got)
	}
	if got := call("/key", tok.AccessToken); got != http.StatusUnauthorized {
		t.Errorf("OAuth token on API-key route = %d", got)
	}
	if got := call("/session", tok.AccessToken); got != http.StatusUnauthorized {
		t.Errorf("OAuth token on session route = %d", got)
	}
	if got := call("/scope", key.Key); got != http.StatusUnauthorized {
		t.Errorf("API key on scope route = %d", got)
	}
}

func TestTokenRateLimit(t *testing.T) {
	e := newEnv(t)
	app := e.createApp(e.user("owner"), "stardance")
	other := e.createApp(e.user("other"), "other")
	form := url.Values{"grant_type": {"client_credentials"}}
	for i := range 60 {
		if rec := e.post("/oauth/token", form, creds(app)); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d", i, rec.Code)
		}
	}
	rec := e.post("/oauth/token", form, creds(app))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("61st = %d %v", rec.Code, rec.Header())
	}
	if rec := e.post("/oauth/revoke", url.Values{"token": {"x"}}, creds(app)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("revoke shares the budget: %d", rec.Code)
	}
	if rec := e.post("/oauth/token", form, creds(other)); rec.Code != http.StatusOK {
		t.Fatalf("another client = %d", rec.Code)
	}
}

func TestOAuthRoutesHaveNoCORS(t *testing.T) {
	e := newEnv(t)
	app := e.createApp(e.user("owner"), "stardance")
	cors := httpapi.CORS(httpapi.CORSConfig{Origin: webURL})(e.mux)
	for _, path := range []string{"/oauth/token", "/oauth/revoke"} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path,
			strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", webURL)
		r.SetBasicAuth(app.ClientID, app.ClientSecret)
		rec := httptest.NewRecorder()
		cors.ServeHTTP(rec, r)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s answered CORS", path)
		}
	}
}

func TestCleanup(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	live := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile")), "")
	consentID(t, e.authorize(e.user("bob"), authParams(app, "profile")))
	e.clock.Advance(oauth.RequestTTL + 25*time.Hour)

	n, err := e.svc.Cleanup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 { // the pending request and the used code
		t.Fatalf("deleted %d rows, want 2", n)
	}
	if rec := e.probe("/probe/profile", live.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("live token after cleanup = %d", rec.Code)
	}
}
