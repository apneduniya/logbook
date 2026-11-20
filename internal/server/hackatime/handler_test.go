package hackatime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

func decodeStatus(t *testing.T, body []byte) StatusJSON {
	t.Helper()
	var s StatusJSON
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("decoding status %s: %v", body, err)
	}
	return s
}

func TestLinkFlow(t *testing.T) {
	e := newEnv(t)
	e.fake.addUser(42, "green")
	alice := e.newUser(t, "alice")
	ctx := context.Background()

	authURL, cookie := e.startLink(t, alice)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/auth/hackatime" || cookie.MaxAge <= 0 {
		t.Fatalf("state cookie attributes = %+v", cookie)
	}
	u, _ := url.Parse(authURL)
	if u.Host != mustURL(t, e.fake.srv.URL).Host {
		t.Fatalf("authorize URL host = %s", u.Host)
	}
	code, state := e.fake.authorize(authURL, 42)
	rec := e.do(t, http.MethodGet, "/auth/hackatime/callback?code="+code+"&state="+state, alice, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?hackatime=linked" {
		t.Fatalf("callback = %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].Name != stateCookie || c[0].MaxAge >= 0 {
		t.Fatalf("callback should clear the state cookie, got %+v", c)
	}

	// Token is stored encrypted and bound to alice.
	link, err := e.st.GetHackatimeLink(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	token := e.tokenFromDB(t, alice)
	if token == "" || string(link.AccessTokenEnc) == token {
		t.Fatal("token stored in plaintext")
	}
	if link.HackatimeUserID != 42 || link.TrustLevel != "green" || link.HackatimeUsername != "gh42" {
		t.Fatalf("link = %+v", link)
	}

	// Import queued and initialised.
	imp, err := e.st.GetHackatimeImport(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Status != importPending || imp.FromDate.Time.Format(time.DateOnly) != "2025-03-01" ||
		imp.ToDate.Time.Format(time.DateOnly) != "2025-03-03" || imp.NextDate != imp.FromDate {
		t.Fatalf("import = %+v", imp)
	}
	if n := e.count(t, "SELECT count(*) FROM river_job WHERE kind = 'hackatime_import' AND args->>'user_id' = $1", alice.String()); n != 1 {
		t.Fatalf("queued import jobs = %d, want 1", n)
	}
	if n := e.count(t, "SELECT count(*) FROM audit_log WHERE action = 'hackatime.linked' AND subject_id = $1", alice.String()); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}

	// Status shows the link, never the token.
	rec = e.do(t, http.MethodGet, "/api/v1/hackatime/link", alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	assertNoSecret(t, "status response", rec.Body.String(), token)
	s := decodeStatus(t, rec.Body.Bytes())
	if !s.Linked || s.HackatimeUserID != 42 || s.TrustLevel != TrustGreen || s.Import == nil ||
		s.Import.Status != "pending" || s.Import.DaysTotal != 3 || s.Import.DaysDone != 0 {
		t.Fatalf("status = %+v", s)
	}
	assertNoSecret(t, "logs", e.logs.String(), token)

	// Relinking the same account is fine and doesn't queue a second job.
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("relink = %d %s", rec.Code, rec.Body)
	}
	if n := e.count(t, "SELECT count(*) FROM river_job WHERE kind = 'hackatime_import'"); n != 1 {
		t.Fatalf("queued import jobs after relink = %d, want 1", n)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCallbackRejects(t *testing.T) {
	e := newEnv(t)
	e.fake.addUser(42, "green")
	alice := e.newUser(t, "alice")
	bob := e.newUser(t, "bob")

	tests := []struct {
		name     string
		run      func(t *testing.T) (code int, location string)
		wantCode int
		wantLoc  string
	}{
		{"wrong state", func(t *testing.T) (int, string) {
			authURL, cookie := e.startLink(t, alice)
			code, _ := e.fake.authorize(authURL, 42)
			rec := e.do(t, http.MethodGet, "/auth/hackatime/callback?code="+code+"&state=forged", alice, cookie)
			return rec.Code, rec.Header().Get("Location")
		}, http.StatusBadRequest, ""},
		{"missing cookie", func(t *testing.T) (int, string) {
			authURL, _ := e.startLink(t, alice)
			code, state := e.fake.authorize(authURL, 42)
			rec := e.do(t, http.MethodGet, "/auth/hackatime/callback?code="+code+"&state="+state, alice)
			return rec.Code, ""
		}, http.StatusBadRequest, ""},
		{"cookie replayed by another user", func(t *testing.T) (int, string) {
			authURL, cookie := e.startLink(t, alice)
			code, state := e.fake.authorize(authURL, 42)
			rec := e.do(t, http.MethodGet, "/auth/hackatime/callback?code="+code+"&state="+state, bob, cookie)
			return rec.Code, ""
		}, http.StatusBadRequest, ""},
		{"PKCE mismatch", func(t *testing.T) (int, string) {
			authURL, cookie := e.startLink(t, alice)
			// An attacker's authorization (different challenge) injected into alice's callback.
			other, _ := e.svc.client.AuthCodeURL("x", "attacker-verifier-attacker-verifier-attacker-v")
			code, _ := e.fake.authorize(other, 42)
			_, state := e.fake.authorize(authURL, 42)
			rec := e.do(t, http.MethodGet, "/auth/hackatime/callback?code="+code+"&state="+state, alice, cookie)
			return rec.Code, ""
		}, http.StatusBadRequest, ""},
		{"expired state", func(t *testing.T) (int, string) {
			authURL, cookie := e.startLink(t, alice)
			code, state := e.fake.authorize(authURL, 42)
			e.advance(stateTTL + time.Minute)
			defer e.advance(-(stateTTL + time.Minute))
			rec := e.do(t, http.MethodGet, "/auth/hackatime/callback?code="+code+"&state="+state, alice, cookie)
			return rec.Code, ""
		}, http.StatusBadRequest, ""},
		{"user denied", func(t *testing.T) (int, string) {
			_, cookie := e.startLink(t, alice)
			rec := e.do(t, http.MethodGet, "/auth/hackatime/callback?error=access_denied", alice, cookie)
			return rec.Code, rec.Header().Get("Location")
		}, http.StatusSeeOther, "/settings?hackatime=denied"},
		{"not signed in", func(t *testing.T) (int, string) {
			rec := e.do(t, http.MethodGet, "/auth/hackatime/callback?code=x&state=y", uuid.Nil)
			return rec.Code, ""
		}, http.StatusUnauthorized, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, loc := tt.run(t)
			if code != tt.wantCode || loc != tt.wantLoc {
				t.Fatalf("got %d %q, want %d %q", code, loc, tt.wantCode, tt.wantLoc)
			}
		})
	}
	for _, u := range []uuid.UUID{alice, bob} {
		if _, err := e.svc.TrustLevel(context.Background(), u); !errors.Is(err, ErrNotLinked) {
			t.Fatalf("user linked after rejected callbacks: %v", err)
		}
	}
}

func TestDoubleLinkConflict(t *testing.T) {
	e := newEnv(t)
	e.fake.addUser(42, "green")
	alice := e.newUser(t, "alice")
	bob := e.newUser(t, "bob")

	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("alice link = %d %s", rec.Code, rec.Body)
	}
	aliceToken := e.tokenFromDB(t, alice)

	rec := e.link(t, bob, 42)
	if rec.Code != http.StatusConflict {
		t.Fatalf("bob link = %d %s, want 409", rec.Code, rec.Body)
	}
	if s := decodeStatus(t, e.do(t, http.MethodGet, "/api/v1/hackatime/link", bob).Body.Bytes()); s.Linked {
		t.Fatalf("bob status = %+v, want unlinked", s)
	}
	if e.tokenFromDB(t, alice) != aliceToken {
		t.Fatal("alice's link changed")
	}
	// Hackatime reuses tokens, so bob's attempt was handed alice's token.
	// Revoking it would break alice's link; it must still work.
	if revoked := e.fake.revokedTokens(); len(revoked) != 0 {
		t.Fatalf("revoked %d tokens on a rejected link, want none", len(revoked))
	}
	if !e.fake.liveToken(aliceToken) {
		t.Fatal("alice's token no longer accepted by Hackatime")
	}
}

// TestRelinkAfterTransientFailureKeepsToken renews a link while Hackatime's
// /me has a hiccup. Hackatime returns the same token as the stored one, which
// must stay usable.
func TestRelinkAfterTransientFailureKeepsToken(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	token := e.tokenFromDB(t, alice)

	e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxRetries = -1 })
	e.svc.client = e.client
	e.fake.setMeHook(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	if rec := e.link(t, alice, 42); rec.Code != http.StatusBadGateway {
		t.Fatalf("relink with /me down = %d %s, want 502", rec.Code, rec.Body)
	}
	e.fake.setMeHook(nil)
	if len(e.fake.revokedTokens()) != 0 || !e.fake.liveToken(token) {
		t.Fatal("failed relink revoked the stored token")
	}
	if got := e.tokenFromDB(t, alice); got != token {
		t.Fatal("stored token changed on a failed relink")
	}
	if me, err := e.client.Me(context.Background(), token); err != nil || me.ID != 42 {
		t.Fatalf("stored token unusable after failed relink: %+v, %v", me, err)
	}
}

// TestSwitchAccountRevokesOldToken relinks to a different Hackatime account:
// the old account's token is revoked and its import restarted.
func TestSwitchAccountRevokesOldToken(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	oldToken := e.tokenFromDB(t, alice)
	e.fake.addUser(43, "green")
	if rec := e.link(t, alice, 43); rec.Code != http.StatusSeeOther {
		t.Fatalf("switch link = %d %s", rec.Code, rec.Body)
	}
	newToken := e.tokenFromDB(t, alice)
	if newToken == oldToken || !e.fake.liveToken(newToken) {
		t.Fatal("new account's token not stored")
	}
	if r := e.fake.revokedTokens(); len(r) != 1 || r[0] != oldToken || e.fake.liveToken(oldToken) {
		t.Fatal("old account's token not revoked")
	}
	if s := decodeStatus(t, e.do(t, http.MethodGet, "/api/v1/hackatime/link", alice).Body.Bytes()); s.HackatimeUserID != 43 {
		t.Fatalf("status = %+v, want account 43", s)
	}
	assertNoSecret(t, "logs", e.logs.String(), oldToken)
	assertNoSecret(t, "logs", e.logs.String(), newToken)
}

// TestBannedLinkSticks proves a red user can't shed the ban by unlinking or
// by swapping in a clean Hackatime account.
func TestBannedLinkSticks(t *testing.T) {
	e := newEnv(t)
	alice := e.newUser(t, "alice")
	e.fake.addUser(42, "red")
	e.fake.addUser(43, "green")
	ctx := context.Background()
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("link = %d %s", rec.Code, rec.Body)
	}
	token := e.tokenFromDB(t, alice)

	if rec := e.do(t, http.MethodDelete, "/api/v1/hackatime/link", alice); rec.Code != http.StatusForbidden {
		t.Fatalf("unlink while banned = %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := e.link(t, alice, 43); rec.Code != http.StatusForbidden {
		t.Fatalf("switch to clean account while banned = %d %s, want 403", rec.Code, rec.Body)
	}
	lvl, err := e.svc.TrustLevel(ctx, alice)
	if err != nil || !lvl.Banned() {
		t.Fatalf("TrustLevel() = %q, %v; want red", lvl, err)
	}
	if s := decodeStatus(t, e.do(t, http.MethodGet, "/api/v1/hackatime/link", alice).Body.Bytes()); s.HackatimeUserID != 42 {
		t.Fatalf("status = %+v, want still linked to 42", s)
	}
	if !e.fake.liveToken(token) || len(e.fake.revokedTokens()) != 0 {
		t.Fatal("banned link's token was revoked")
	}
	// Renewing the same (banned) account is still allowed.
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("relink same banned account = %d %s", rec.Code, rec.Body)
	}
	// Once Hackatime clears the user, unlinking works again.
	e.fake.setTrust(42, "green")
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("relink = %d", rec.Code)
	}
	if rec := e.do(t, http.MethodDelete, "/api/v1/hackatime/link", alice); rec.Code != http.StatusNoContent {
		t.Fatalf("unlink after clearance = %d %s", rec.Code, rec.Body)
	}
}

func TestUnlink(t *testing.T) {
	e := newEnv(t)
	e.fake.addUser(42, "green")
	alice := e.newUser(t, "alice")
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("link = %d", rec.Code)
	}
	token := e.tokenFromDB(t, alice)

	rec := e.do(t, http.MethodDelete, "/api/v1/hackatime/link", alice)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unlink = %d %s", rec.Code, rec.Body)
	}
	if n := e.count(t, "SELECT count(*) FROM hackatime_links WHERE user_id = $1", alice); n != 0 {
		t.Fatal("link row survived unlink")
	}
	if n := e.count(t, "SELECT count(*) FROM hackatime_imports WHERE user_id = $1", alice); n != 0 {
		t.Fatal("import row survived unlink")
	}
	if n := e.count(t, "SELECT count(*) FROM audit_log WHERE action = 'hackatime.unlinked' AND subject_id = $1", alice.String()); n != 1 {
		t.Fatalf("unlink audit rows = %d", n)
	}
	if r := e.fake.revokedTokens(); len(r) != 1 || r[0] != token {
		t.Fatal("token not revoked at Hackatime")
	}
	if s := decodeStatus(t, e.do(t, http.MethodGet, "/api/v1/hackatime/link", alice).Body.Bytes()); s.Linked {
		t.Fatal("still linked after unlink")
	}
	if rec := e.do(t, http.MethodDelete, "/api/v1/hackatime/link", alice); rec.Code != http.StatusNotFound {
		t.Fatalf("second unlink = %d, want 404", rec.Code)
	}
	assertNoSecret(t, "logs", e.logs.String(), token)

	// After unlinking, another user may link the account.
	bob := e.newUser(t, "bob")
	if rec := e.link(t, bob, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("bob link after alice unlinked = %d %s", rec.Code, rec.Body)
	}
}

func TestCrossUserIsolation(t *testing.T) {
	e := newEnv(t)
	e.fake.addUser(42, "green")
	alice := e.newUser(t, "alice")
	mallory := e.newUser(t, "mallory")
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("link = %d", rec.Code)
	}

	rec := e.do(t, http.MethodGet, "/api/v1/hackatime/link", mallory)
	if rec.Code != http.StatusOK {
		t.Fatalf("mallory status = %d", rec.Code)
	}
	if s := decodeStatus(t, rec.Body.Bytes()); s.Linked || s.HackatimeUserID != 0 || s.Import != nil {
		t.Fatalf("mallory sees %+v", s)
	}
	if rec := e.do(t, http.MethodDelete, "/api/v1/hackatime/link", mallory); rec.Code != http.StatusNotFound {
		t.Fatalf("mallory unlink = %d, want 404", rec.Code)
	}
	if s := decodeStatus(t, e.do(t, http.MethodGet, "/api/v1/hackatime/link", alice).Body.Bytes()); !s.Linked {
		t.Fatal("alice lost her link")
	}
	if _, err := e.svc.TrustLevel(context.Background(), mallory); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("TrustLevel(mallory) err = %v", err)
	}

	// Copying alice's encrypted token onto another row makes it unreadable.
	if _, err := e.st.Pool.Exec(context.Background(),
		`INSERT INTO hackatime_links (user_id, hackatime_user_id, access_token_enc)
		 SELECT $1, 99, access_token_enc FROM hackatime_links WHERE user_id = $2`, mallory, alice); err != nil {
		t.Fatal(err)
	}
	link, err := e.st.GetHackatimeLink(context.Background(), mallory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.box.Open(link.AccessTokenEnc, mallory[:]); err == nil {
		t.Fatal("alice's token decrypted under mallory's id")
	}
}

func TestStatusMasksSuspectedTrust(t *testing.T) {
	e := newEnv(t)
	e.fake.addUser(42, "yellow")
	alice := e.newUser(t, "alice")
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("link = %d", rec.Code)
	}
	if s := decodeStatus(t, e.do(t, http.MethodGet, "/api/v1/hackatime/link", alice).Body.Bytes()); s.TrustLevel != TrustBlue {
		t.Fatalf("status trust = %q, want blue (masked)", s.TrustLevel)
	}
	lvl, err := e.svc.TrustLevel(context.Background(), alice)
	if err != nil || lvl != TrustYellow {
		t.Fatalf("TrustLevel() = %q, %v; want raw yellow for gating", lvl, err)
	}
}

func TestEndpointsRequireSession(t *testing.T) {
	e := newEnv(t)
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/auth/hackatime/link"},
		{http.MethodGet, "/auth/hackatime/callback"},
		{http.MethodGet, "/api/v1/hackatime/link"},
		{http.MethodDelete, "/api/v1/hackatime/link"},
	} {
		if rec := e.do(t, r.method, r.path, uuid.Nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", r.method, r.path, rec.Code)
		}
	}
}

func TestLinkNotConfigured(t *testing.T) {
	e := newEnv(t)
	c, err := NewClient(ClientConfig{BaseURL: e.fake.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewHandler(NewService(e.st, e.box, c, nil, ServiceOptions{}), stubSession, HandlerOptions{}).Register(mux)
	e.mux = mux
	if rec := e.do(t, http.MethodGet, "/auth/hackatime/link", e.newUser(t, "alice")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("link = %d, want 503", rec.Code)
	}
}
