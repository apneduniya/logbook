package oauth_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/oauth"
)

func TestCodeFlowWithoutPKCE(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")

	loc := e.authorize(alice, authParams(app, "profile read"))
	id := consentID(t, loc)

	// The consent page reads the pending request.
	rec := e.do(http.MethodGet, "/api/v1/oauth/requests/"+id, alice, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("consent request = %d %s", rec.Code, rec.Body)
	}
	cr := decode[oauth.ConsentRequest](t, rec)
	if cr.App.Name != "App stardance" || cr.App.OwnerUsername != "owner" || len(cr.Scopes) != 2 ||
		cr.Scopes[0].Description == "" || cr.RedirectURI != callback || cr.AlreadyGranted {
		t.Fatalf("consent request = %+v", cr)
	}

	back := e.decide(alice, id, true)
	if back.Query().Get("state") != "xyz" || back.Query().Get("code") == "" {
		t.Fatalf("redirect = %s", back)
	}
	// The request is single use.
	if rec := e.do(http.MethodPost, "/api/v1/oauth/requests/"+id, alice, `{"approve":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("second decision = %d, want 404", rec.Code)
	}

	tok := e.mustExchange(app, back.Query().Get("code"), "")
	if tok.TokenType != "Bearer" || tok.Scope != "profile read" || tok.ExpiresIn != int64(oauth.AccessTokenTTL/time.Second) ||
		tok.RefreshToken == "" || tok.CreatedAt != e.clock.Now().Unix() || !strings.HasPrefix(tok.AccessToken, "lbo_") {
		t.Fatalf("token = %+v", tok)
	}
	for _, p := range []string{"/probe/profile", "/probe/read"} {
		rec := e.probe(p, tok.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", p, rec.Code, rec.Body)
		}
		got := decode[probeJSON](t, rec)
		if got.User != alice.String() || got.Via != "oauth" || got.App != app.ID || got.Client {
			t.Fatalf("%s = %+v", p, got)
		}
	}
	if rec := e.probe("/probe/submissions", tok.AccessToken); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Header().Get("WWW-Authenticate"), "insufficient_scope") {
		t.Fatalf("user token on submissions = %d %v", rec.Code, rec.Header())
	}

	// Only hashes are stored.
	if n := e.count("SELECT count(*) FROM oauth_tokens WHERE access_hash = $1 OR refresh_hash = $2",
		[]byte(tok.AccessToken), []byte(tok.RefreshToken)); n != 0 {
		t.Fatal("plaintext token stored")
	}
	if got := actions(t, e.st, alice); len(got) != 1 || got[0] != "oauth.grant" {
		t.Fatalf("alice's audit = %v", got)
	}
}

func TestProfileOnlyTokenCantRead(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "flavortown")
	tok := e.mustExchange(app, e.codeFor(alice, authParams(app, "")), "") // default scope
	if tok.Scope != "profile" {
		t.Fatalf("default scope = %q", tok.Scope)
	}
	if rec := e.probe("/probe/read", tok.AccessToken); rec.Code != http.StatusForbidden {
		t.Fatalf("read with profile token = %d", rec.Code)
	}
	if rec := e.probe("/probe/profile", tok.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("profile = %d", rec.Code)
	}
}

func TestCodeFlowWithPKCEPublicClient(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createAppWith(owner, map[string]any{
		"name": "Desktop", "slug": "desktop", "redirect_uris": []string{callback}, "confidential": false,
	})
	if app.ClientSecret != "" || app.Confidential {
		t.Fatalf("public client got a secret: %+v", app)
	}
	verifier, challenge := pkcePair(t)
	q := authParams(app, "read")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")

	// A wrong verifier fails and uses the code up.
	code := e.codeFor(alice, q)
	wrong, _ := pkcePair(t)
	expectOAuthError(t, e.exchange(app, code, wrong), http.StatusBadRequest, "invalid_grant")
	expectOAuthError(t, e.exchange(app, code, verifier), http.StatusBadRequest, "invalid_grant")

	// A missing verifier fails.
	code = e.codeFor(alice, q) // the grant now exists, so no consent is asked
	expectOAuthError(t, e.exchange(app, code, ""), http.StatusBadRequest, "invalid_grant")

	// The right verifier works.
	code = e.codeFor(alice, q)
	tok := e.mustExchange(app, code, verifier)
	if rec := e.probe("/probe/read", tok.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("read = %d", rec.Code)
	}

	// A public client must not send a secret.
	form := url.Values{"grant_type": {"authorization_code"}, "code": {e.codeFor(alice, q)}, "redirect_uri": {callback},
		"code_verifier": {verifier}, "client_id": {app.ClientID}, "client_secret": {"lbs_guess"}}
	expectOAuthError(t, e.post("/oauth/token", form, nil), http.StatusUnauthorized, "invalid_client")
}

func TestConfidentialClientWithPKCE(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "savepoint")
	verifier, challenge := pkcePair(t)
	q := authParams(app, "profile")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	e.mustExchange(app, e.codeFor(alice, q), verifier)

	// A verifier on a code that had no challenge is refused (no downgrade).
	expectOAuthError(t, e.exchange(app, e.codeFor(alice, authParams(app, "profile")), verifier),
		http.StatusBadRequest, "invalid_grant")
}

func TestPublicClientWithoutPKCERefused(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createAppWith(owner, map[string]any{
		"name": "Desktop", "slug": "desktop", "redirect_uris": []string{callback}, "confidential": false,
	})
	loc := e.authorize(alice, authParams(app, "profile"))
	if loc.String()[:len(callback)] != callback || loc.Query().Get("error") != "invalid_request" || loc.Query().Get("state") != "xyz" {
		t.Fatalf("redirect = %s, want invalid_request at the callback", loc)
	}
	if n := e.count("SELECT count(*) FROM oauth_requests"); n != 0 {
		t.Fatalf("%d pending requests stored", n)
	}
	// plain is not supported either.
	q := authParams(app, "profile")
	q.Set("code_challenge", strings.Repeat("a", 43))
	q.Set("code_challenge_method", "plain")
	if loc := e.authorize(alice, q); loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("plain challenge: %s", loc)
	}
	// Client credentials need a confidential client.
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {app.ClientID}}
	expectOAuthError(t, e.post("/oauth/token", form, nil), http.StatusBadRequest, "unauthorized_client")
}

func TestCodeReuseRevokesItsTokens(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	code := e.codeFor(alice, authParams(app, "profile read"))
	tok := e.mustExchange(app, code, "")

	// A second token from another code is unaffected.
	other := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile")), "")

	expectOAuthError(t, e.exchange(app, code, ""), http.StatusBadRequest, "invalid_grant")
	if rec := e.probe("/probe/profile", tok.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token from the replayed code = %d, want 401", rec.Code)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE action = 'oauth.revoke' AND data->>'reason' = 'code_reuse'`); n != 1 {
		t.Fatalf("code_reuse audit rows = %d", n)
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}
	expectOAuthError(t, e.post("/oauth/token", refresh, creds(app)), http.StatusBadRequest, "invalid_grant")
	if rec := e.probe("/probe/profile", other.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("token from another code = %d, want 200", rec.Code)
	}
}

func TestExpiredCodeRefused(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	code := e.codeFor(alice, authParams(app, "profile"))
	e.clock.Advance(oauth.CodeTTL)
	expectOAuthError(t, e.exchange(app, code, ""), http.StatusBadRequest, "invalid_grant")
}

func TestRedirectURIRules(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")

	for _, bad := range []string{"", "https://evil.test/callback", callback + "/", callback + "?x=1", "HTTPS://program.test/callback"} {
		q := authParams(app, "profile")
		q.Set("redirect_uri", bad)
		loc := e.authorize(alice, q)
		if loc.Scheme+"://"+loc.Host != webURL || loc.Query().Get("error") != "invalid_redirect_uri" {
			t.Errorf("redirect_uri %q: sent to %s, want the web error page", bad, loc)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", "6f1c2c7e-0000-4000-8000-000000000000"} {
		q := authParams(app, "profile")
		q.Set("client_id", bad)
		loc := e.authorize(alice, q)
		if loc.Scheme+"://"+loc.Host != webURL || loc.Query().Get("error") != "invalid_client" {
			t.Errorf("client_id %q: sent to %s, want the web error page", bad, loc)
		}
	}

	// Other errors go back to the verified redirect URI with the state.
	q := authParams(app, "profile admin")
	if loc := e.authorize(alice, q); loc.Query().Get("error") != "invalid_scope" || loc.Query().Get("state") != "xyz" {
		t.Errorf("bad scope: %s", loc)
	}
	q = authParams(app, "profile")
	q.Set("response_type", "token")
	if loc := e.authorize(alice, q); loc.Query().Get("error") != "unsupported_response_type" {
		t.Errorf("implicit grant: %s", loc)
	}

	// The token exchange needs the same redirect URI, and a mismatch uses the code up.
	code := e.codeFor(alice, authParams(app, "profile"))
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {devCallback}}
	expectOAuthError(t, e.post("/oauth/token", form, creds(app)), http.StatusBadRequest, "invalid_grant")
	expectOAuthError(t, e.exchange(app, code, ""), http.StatusBadRequest, "invalid_grant")

	// A second registered URI works when used on both legs.
	q = authParams(app, "profile")
	q.Set("redirect_uri", devCallback)
	code = e.codeFor(alice, q)
	form = url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {devCallback}}
	if rec := e.post("/oauth/token", form, creds(app)); rec.Code != http.StatusOK {
		t.Fatalf("dev callback = %d %s", rec.Code, rec.Body)
	}
}

func TestClientAuthentication(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	other := e.createApp(owner, "other")
	code := e.codeFor(alice, authParams(app, "profile"))
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback}}

	rec := e.post("/oauth/token", form, &[2]string{app.ClientID, app.ClientSecret + "x"})
	expectOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("Basic failure without WWW-Authenticate")
	}
	expectOAuthError(t, e.post("/oauth/token", form, &[2]string{app.ClientID, ""}), http.StatusUnauthorized, "invalid_client")
	noSecret := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback}, "client_id": {app.ClientID}}
	expectOAuthError(t, e.post("/oauth/token", noSecret, nil), http.StatusUnauthorized, "invalid_client")
	// Another client can't redeem the code, and its attempt doesn't burn it.
	expectOAuthError(t, e.post("/oauth/token", form, creds(other)), http.StatusBadRequest, "invalid_grant")

	// Credentials in the body work as well as Basic.
	body := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback},
		"client_id": {app.ClientID}, "client_secret": {app.ClientSecret}}
	if rec := e.post("/oauth/token", body, nil); rec.Code != http.StatusOK {
		t.Fatalf("body credentials = %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("token error without Cache-Control: no-store")
	}
}

func TestTokenEndpointAcceptsJSON(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	body := mustJSON(map[string]string{
		"grant_type": "authorization_code", "code": e.codeFor(alice, authParams(app, "profile")),
		"redirect_uri": callback, "client_id": app.ClientID, "client_secret": app.ClientSecret,
	})
	if rec := e.do(http.MethodPost, "/oauth/token", [16]byte{}, body); rec.Code != http.StatusOK {
		t.Fatalf("JSON token request = %d %s", rec.Code, rec.Body)
	}
}

func TestRefreshRotation(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	first := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile read")), "")

	e.clock.Advance(time.Hour)
	rec := e.post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}}, creds(app))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body)
	}
	second := decode[tokenJSON](t, rec)
	if second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken || second.Scope != "profile read" {
		t.Fatalf("refresh = %+v", second)
	}
	if rec := e.probe("/probe/read", first.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old access token = %d, want 401", rec.Code)
	}
	if rec := e.probe("/probe/read", second.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("new access token = %d", rec.Code)
	}

	// Narrowing is allowed, widening isn't.
	rec = e.post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {second.RefreshToken},
		"scope": {"profile"}}, creds(app))
	if rec.Code != http.StatusOK {
		t.Fatalf("narrowing refresh = %d %s", rec.Code, rec.Body)
	}
	third := decode[tokenJSON](t, rec)
	if third.Scope != "profile" {
		t.Fatalf("narrowed scope = %q", third.Scope)
	}
	expectOAuthError(t, e.post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {third.RefreshToken},
		"scope": {"profile read"}}, creds(app)), http.StatusBadRequest, "invalid_scope")

	// Another client can't use the refresh token.
	other := e.createApp(owner, "other")
	expectOAuthError(t, e.post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {third.RefreshToken}},
		creds(other)), http.StatusBadRequest, "invalid_grant")
	if rec := e.probe("/probe/profile", third.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("token after another client's attempt = %d", rec.Code)
	}

	// Replaying a rotated refresh token revokes the whole family.
	expectOAuthError(t, e.post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}},
		creds(app)), http.StatusBadRequest, "invalid_grant")
	if rec := e.probe("/probe/profile", third.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("family after refresh replay = %d, want 401", rec.Code)
	}
}

func TestAccessTokenExpires(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	tok := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile")), "")
	e.clock.Advance(oauth.AccessTokenTTL)
	if rec := e.probe("/probe/profile", tok.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d", rec.Code)
	}
	// The refresh token outlives it.
	rec := e.post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}, creds(app))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh after expiry = %d %s", rec.Code, rec.Body)
	}
}

func TestClientCredentials(t *testing.T) {
	e := newEnv(t)
	owner := e.user("owner")
	app := e.createApp(owner, "stardance")

	for _, scope := range []string{"profile", "read", "submissions read", "admin"} {
		form := url.Values{"grant_type": {"client_credentials"}, "scope": {scope}}
		expectOAuthError(t, e.post("/oauth/token", form, creds(app)), http.StatusBadRequest, "invalid_scope")
	}
	rec := e.post("/oauth/token", url.Values{"grant_type": {"client_credentials"}}, creds(app))
	if rec.Code != http.StatusOK {
		t.Fatalf("client credentials = %d %s", rec.Code, rec.Body)
	}
	tok := decode[tokenJSON](t, rec)
	if tok.Scope != "submissions" || tok.RefreshToken != "" || tok.ExpiresIn != int64(oauth.ClientTokenTTL/time.Second) {
		t.Fatalf("token = %+v", tok)
	}
	for _, p := range []string{"/probe/profile", "/probe/read"} {
		if rec := e.probe(p, tok.AccessToken); rec.Code != http.StatusForbidden {
			t.Errorf("client token on %s = %d, want 403", p, rec.Code)
		}
	}
	rec = e.probe("/probe/submissions", tok.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("submissions = %d %s", rec.Code, rec.Body)
	}
	got := decode[probeJSON](t, rec)
	if got.User != "" || !got.Client || got.App != app.ID {
		t.Fatalf("client token context = %+v", got)
	}
	// It never reads a user through the optional middleware either.
	if got := decode[probeJSON](t, e.probe("/probe/optional", tok.AccessToken)); got.User != "" || got.App != "" {
		t.Fatalf("optional with client token = %+v", got)
	}
}

func TestUnsupportedGrants(t *testing.T) {
	e := newEnv(t)
	app := e.createApp(e.user("owner"), "stardance")
	expectOAuthError(t, e.post("/oauth/token", url.Values{"grant_type": {"password"}}, creds(app)),
		http.StatusBadRequest, "unsupported_grant_type")
	expectOAuthError(t, e.post("/oauth/token", url.Values{}, creds(app)), http.StatusBadRequest, "invalid_request")
	expectOAuthError(t, e.post("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "redirect_uri": {callback}}, creds(app)),
		http.StatusBadRequest, "invalid_request")
}

func TestPriorGrantSkipsConsent(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	e.codeFor(alice, authParams(app, "profile read"))

	// Same or fewer scopes: straight back with a code.
	for _, scope := range []string{"profile read", "read", ""} {
		loc := e.authorize(alice, authParams(app, scope))
		if loc.Query().Get("code") == "" || loc.Query().Get("state") != "xyz" {
			t.Fatalf("scope %q: redirect = %s, want a code", scope, loc)
		}
	}
	// Signed out: consent page (the web UI signs the user in first), which
	// says the scopes were granted before.
	id := consentID(t, e.authorize([16]byte{}, authParams(app, "profile")))
	cr := decode[oauth.ConsentRequest](t, e.do(http.MethodGet, "/api/v1/oauth/requests/"+id, alice, ""))
	if !cr.AlreadyGranted {
		t.Fatalf("consent request = %+v, want already_granted", cr)
	}

	// Another user still has to consent.
	bob := e.user("bob")
	consentID(t, e.authorize(bob, authParams(app, "profile")))
}

func TestNewScopeNeedsConsentAndAddsToGrant(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	e.codeFor(alice, authParams(app, "profile"))
	id := consentID(t, e.authorize(alice, authParams(app, "read")))
	e.decide(alice, id, true)
	rec := e.do(http.MethodGet, "/api/v1/me/grants", alice, "")
	gs := decode[map[string][]oauth.Grant](t, rec)["grants"]
	if len(gs) != 1 || strings.Join(gs[0].Scopes, " ") != "profile read" {
		t.Fatalf("grants = %+v", gs)
	}
}

func TestDenyConsent(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	loc := e.decide(alice, consentID(t, e.authorize(alice, authParams(app, "profile"))), false)
	if loc.Query().Get("error") != "access_denied" || loc.Query().Get("state") != "xyz" || loc.Query().Get("code") != "" {
		t.Fatalf("deny redirect = %s", loc)
	}
	if n := e.count("SELECT count(*) FROM oauth_grants"); n != 0 {
		t.Fatalf("%d grants after denial", n)
	}
}

func TestConsentRequestBelongsToOneUser(t *testing.T) {
	e := newEnv(t)
	owner, alice, bob := e.user("owner"), e.user("alice"), e.user("bob")
	app := e.createApp(owner, "stardance")

	// Started signed in: bound to alice at once.
	id := consentID(t, e.authorize(alice, authParams(app, "profile")))
	if rec := e.do(http.MethodGet, "/api/v1/oauth/requests/"+id, bob, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob reads alice's request = %d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/v1/oauth/requests/"+id, bob, `{"approve":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("bob approves alice's request = %d", rec.Code)
	}

	// Started signed out: bound to the first user who opens it.
	id = consentID(t, e.authorize([16]byte{}, authParams(app, "profile")))
	if rec := e.do(http.MethodGet, "/api/v1/oauth/requests/"+id, alice, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice opens = %d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/v1/oauth/requests/"+id, bob, `{"approve":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("bob approves after alice opened = %d", rec.Code)
	}

	// Unknown, malformed and expired ids.
	for _, bad := range []string{"nope", strings.Repeat("A", 43)} {
		if rec := e.do(http.MethodGet, "/api/v1/oauth/requests/"+bad, alice, ""); rec.Code != http.StatusNotFound {
			t.Errorf("request %q = %d", bad, rec.Code)
		}
	}
	id = consentID(t, e.authorize(alice, authParams(app, "profile")))
	e.clock.Advance(oauth.RequestTTL)
	if rec := e.do(http.MethodPost, "/api/v1/oauth/requests/"+id, alice, `{"approve":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("expired request = %d", rec.Code)
	}
	// approve must be given.
	id = consentID(t, e.authorize(alice, authParams(app, "profile")))
	if rec := e.do(http.MethodPost, "/api/v1/oauth/requests/"+id, alice, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing approve = %d", rec.Code)
	}
}

// TestConsentAfterRedirectURIRemoved: once the owner deregisters the
// redirect URI a pending request names, neither the consent page nor the
// decision (approve or deny) sends the browser there.
func TestConsentAfterRedirectURIRemoved(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	approveID := consentID(t, e.authorize(alice, authParams(app, "profile")))
	denyID := consentID(t, e.authorize(alice, authParams(app, "profile read")))

	if rec := e.do(http.MethodPatch, "/api/v1/me/apps/"+app.ID, owner,
		mustJSON(map[string]any{"redirect_uris": []string{devCallback}})); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(http.MethodGet, "/api/v1/oauth/requests/"+approveID, alice, ""); rec.Code != http.StatusNotFound {
		t.Errorf("consent request = %d %s, want 404", rec.Code, rec.Body)
	}
	for id, approve := range map[string]bool{approveID: true, denyID: false} {
		rec := e.do(http.MethodPost, "/api/v1/oauth/requests/"+id, alice, mustJSON(map[string]bool{"approve": approve}))
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "program.test") {
			t.Errorf("decide approve=%v = %d %s, want 404", approve, rec.Code, rec.Body)
		}
	}
	if n := e.count(`SELECT count(*)::int FROM oauth_codes`); n != 0 {
		t.Errorf("%d codes issued for a deregistered redirect URI", n)
	}
	if n := e.count(`SELECT count(*)::int FROM oauth_grants`); n != 0 {
		t.Errorf("%d grants recorded for a deregistered redirect URI", n)
	}
}
