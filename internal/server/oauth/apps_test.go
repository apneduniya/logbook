package oauth_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/server/oauth"
)

func TestCreateApp(t *testing.T) {
	e := newEnv(t)
	owner := e.user("owner")
	app := e.createApp(owner, "stardance")
	if app.ClientID != app.ID || !strings.HasPrefix(app.ClientSecret, "lbs_") || !app.Confidential ||
		app.Role != "owner" || app.Public || len(app.RedirectURIs) != 2 {
		t.Fatalf("app = %+v", app)
	}
	if n := e.count("SELECT count(*) FROM oauth_apps WHERE secret_hash = $1", []byte(app.ClientSecret)); n != 0 {
		t.Fatal("plaintext secret stored")
	}
	list := decode[map[string][]appJSON](t, e.do(http.MethodGet, "/api/v1/me/apps", owner, ""))["apps"]
	if len(list) != 1 || list[0].ID != app.ID || list[0].ClientSecret != "" {
		t.Fatalf("list = %+v", list)
	}
	if got := actions(t, e.st, owner); len(got) != 1 || got[0] != "app.create" {
		t.Fatalf("audit = %v", got)
	}
	if n := e.count("SELECT count(*) FROM audit_log WHERE data::text LIKE '%' || $1 || '%'", app.ClientSecret); n != 0 {
		t.Fatal("audit log contains the secret")
	}
}

func TestCreateAppValidation(t *testing.T) {
	e := newEnv(t)
	owner := e.user("owner")
	e.createApp(owner, "taken")
	base := func() map[string]any {
		return map[string]any{"name": "App", "slug": "ok-slug", "redirect_uris": []string{callback}}
	}
	tests := []struct {
		name string
		edit func(m map[string]any)
		want int
	}{
		{"empty name", func(m map[string]any) { m["name"] = "  " }, http.StatusBadRequest},
		{"long name", func(m map[string]any) { m["name"] = strings.Repeat("a", 65) }, http.StatusBadRequest},
		{"control char", func(m map[string]any) { m["name"] = "a\x00b" }, http.StatusBadRequest},
		{"short slug", func(m map[string]any) { m["slug"] = "a" }, http.StatusBadRequest},
		{"upper slug", func(m map[string]any) { m["slug"] = "Stardance" }, http.StatusBadRequest},
		{"long slug", func(m map[string]any) { m["slug"] = strings.Repeat("a", 41) }, http.StatusBadRequest},
		{"taken slug", func(m map[string]any) { m["slug"] = "taken" }, http.StatusConflict},
		{"no redirect", func(m map[string]any) { m["redirect_uris"] = []string{} }, http.StatusBadRequest},
		{"six redirects", func(m map[string]any) {
			m["redirect_uris"] = []string{"https://a.test/1", "https://a.test/2", "https://a.test/3", "https://a.test/4", "https://a.test/5", "https://a.test/6"}
		}, http.StatusBadRequest},
		{"http redirect", func(m map[string]any) { m["redirect_uris"] = []string{"http://program.test/cb"} }, http.StatusBadRequest},
		{"fragment", func(m map[string]any) { m["redirect_uris"] = []string{callback + "#x"} }, http.StatusBadRequest},
		{"relative", func(m map[string]any) { m["redirect_uris"] = []string{"/callback"} }, http.StatusBadRequest},
		{"custom scheme", func(m map[string]any) { m["redirect_uris"] = []string{"myapp://cb"} }, http.StatusBadRequest},
		{"bad homepage", func(m map[string]any) { m["homepage_url"] = "javascript:alert(1)" }, http.StatusBadRequest},
		{"localhost redirect", func(m map[string]any) { m["redirect_uris"] = []string{"http://127.0.0.1:8080/cb"} }, http.StatusCreated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.edit(m)
			if rec := e.do(http.MethodPost, "/api/v1/me/apps", owner, mustJSON(m)); rec.Code != tc.want {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
}

func TestAppCapPerUser(t *testing.T) {
	e := newEnv(t)
	owner := e.user("owner")
	var first appJSON
	for i := range oauth.MaxAppsPerUser {
		a := e.createApp(owner, fmt.Sprintf("app-%d", i))
		if i == 0 {
			first = a
		}
	}
	body := mustJSON(map[string]any{"name": "One more", "slug": "one-more", "redirect_uris": []string{callback}})
	if rec := e.do(http.MethodPost, "/api/v1/me/apps", owner, body); rec.Code != http.StatusConflict {
		t.Fatalf("11th app = %d %s, want 409", rec.Code, rec.Body)
	}
	// Deleting one frees a place; its slug stays taken.
	if rec := e.do(http.MethodDelete, "/api/v1/me/apps/"+first.ID, owner, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/v1/me/apps", owner, body); rec.Code != http.StatusCreated {
		t.Fatalf("after delete = %d %s", rec.Code, rec.Body)
	}
	reuse := mustJSON(map[string]any{"name": "Reuse", "slug": "app-0", "redirect_uris": []string{callback}})
	if rec := e.do(http.MethodPost, "/api/v1/me/apps", e.user("other"), reuse); rec.Code != http.StatusConflict {
		t.Fatalf("deleted app's slug = %d, want 409", rec.Code)
	}
}

func TestAnotherUsersAppCantBeTouched(t *testing.T) {
	e := newEnv(t)
	alice, mallory := e.user("alice"), e.user("mallory")
	app := e.createApp(alice, "stardance")
	base := "/api/v1/me/apps/" + app.ID
	calls := []struct{ method, path, body string }{
		{http.MethodGet, base, ""},
		{http.MethodPatch, base, `{"name":"pwned"}`},
		{http.MethodPost, base + "/secret", ""},
		{http.MethodDelete, base, ""},
		{http.MethodGet, base + "/members", ""},
		{http.MethodPost, base + "/members", `{"username":"mallory"}`},
		{http.MethodDelete, base + "/members/alice", ""},
	}
	for _, c := range calls {
		if rec := e.do(c.method, c.path, mallory, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("mallory %s %s = %d %s, want 404", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if list := decode[map[string][]appJSON](t, e.do(http.MethodGet, "/api/v1/me/apps", mallory, ""))["apps"]; len(list) != 0 {
		t.Fatalf("mallory lists %+v", list)
	}
	// Nothing changed.
	got := decode[appJSON](t, e.do(http.MethodGet, base, alice, ""))
	if got.Name != "App stardance" {
		t.Fatalf("app = %+v", got)
	}
	tok := e.post("/oauth/token", url.Values{"grant_type": {"client_credentials"}}, creds(app))
	if tok.Code != http.StatusOK {
		t.Fatalf("secret no longer works: %d", tok.Code)
	}
	if got := actions(t, e.st, mallory); len(got) != 0 {
		t.Fatalf("mallory's audit = %v", got)
	}
}

func TestUpdateApp(t *testing.T) {
	e := newEnv(t)
	owner := e.user("owner")
	app := e.createApp(owner, "stardance")
	rec := e.do(http.MethodPatch, "/api/v1/me/apps/"+app.ID, owner,
		`{"name":"Stardance","redirect_uris":["https://new.test/cb"],"public":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}
	got := decode[appJSON](t, rec)
	if got.Name != "Stardance" || !got.Public || len(got.RedirectURIs) != 1 || got.Slug != "stardance" {
		t.Fatalf("patched = %+v", got)
	}
	for _, body := range []string{`{"slug":"new"}`, `{"confidential":false}`, `{"redirect_uris":[]}`, `{"name":""}`} {
		if rec := e.do(http.MethodPatch, "/api/v1/me/apps/"+app.ID, owner, body); rec.Code != http.StatusBadRequest {
			t.Errorf("patch %s = %d, want 400", body, rec.Code)
		}
	}
}

func TestRotateSecret(t *testing.T) {
	e := newEnv(t)
	owner := e.user("owner")
	app := e.createApp(owner, "stardance")
	rec := e.do(http.MethodPost, "/api/v1/me/apps/"+app.ID+"/secret", owner, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body)
	}
	rotated := decode[appJSON](t, rec)
	if rotated.ClientSecret == "" || rotated.ClientSecret == app.ClientSecret {
		t.Fatalf("rotated = %+v", rotated)
	}
	cc := url.Values{"grant_type": {"client_credentials"}}
	expectOAuthError(t, e.post("/oauth/token", cc, creds(app)), http.StatusUnauthorized, "invalid_client")
	if rec := e.post("/oauth/token", cc, creds(rotated)); rec.Code != http.StatusOK {
		t.Fatalf("new secret = %d", rec.Code)
	}
	if got := actions(t, e.st, owner); got[len(got)-1] != "app.secret_rotate" {
		t.Fatalf("audit = %v", got)
	}

	public := e.createAppWith(owner, map[string]any{"name": "P", "slug": "public-client", "redirect_uris": []string{callback}, "confidential": false})
	if rec := e.do(http.MethodPost, "/api/v1/me/apps/"+public.ID+"/secret", owner, ""); rec.Code != http.StatusConflict {
		t.Fatalf("rotate public client = %d, want 409", rec.Code)
	}
}

func TestMembers(t *testing.T) {
	e := newEnv(t)
	owner, rita, carol := e.user("owner"), e.user("rita"), e.user("carol")
	app := e.createApp(owner, "stardance")
	members := "/api/v1/me/apps/" + app.ID + "/members"

	// Nobody can add themselves to someone else's program.
	if rec := e.do(http.MethodPost, members, carol, `{"username":"carol"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("carol adds herself = %d", rec.Code)
	}
	rec := e.do(http.MethodPost, members, owner, `{"username":"RITA"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add rita = %d %s", rec.Code, rec.Body)
	}
	if m := decode[oauth.Member](t, rec); m.Username != "rita" || m.Role != "reviewer" {
		t.Fatalf("member = %+v", m)
	}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"username":"rita"}`, http.StatusConflict},
		{`{"username":"owner"}`, http.StatusConflict},
		{`{"username":"ghost"}`, http.StatusNotFound},
		{`{"username":""}`, http.StatusBadRequest},
	} {
		if rec := e.do(http.MethodPost, members, owner, tc.body); rec.Code != tc.want {
			t.Errorf("add %s = %d, want %d", tc.body, rec.Code, tc.want)
		}
	}

	// A reviewer sees the app and its members, but can't change anything.
	list := decode[map[string][]appJSON](t, e.do(http.MethodGet, "/api/v1/me/apps", rita, ""))["apps"]
	if len(list) != 1 || list[0].Role != "reviewer" {
		t.Fatalf("rita's apps = %+v", list)
	}
	ms := decode[map[string][]oauth.Member](t, e.do(http.MethodGet, members, rita, ""))["members"]
	if len(ms) != 2 || ms[0].Username != "owner" || ms[0].Role != "owner" || ms[1].Username != "rita" {
		t.Fatalf("members = %+v", ms)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, members, `{"username":"carol"}`},
		{http.MethodDelete, members + "/owner", ""},
		{http.MethodDelete, members + "/rita", ""},
		{http.MethodPatch, "/api/v1/me/apps/" + app.ID, `{"public":true}`},
		{http.MethodPost, "/api/v1/me/apps/" + app.ID + "/secret", ""},
		{http.MethodDelete, "/api/v1/me/apps/" + app.ID, ""},
	} {
		if rec := e.do(c.method, c.path, rita, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("reviewer %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
	if rec := e.do(http.MethodGet, "/api/v1/me/apps/"+app.ID, rita, ""); rec.Code != http.StatusOK ||
		decode[appJSON](t, rec).ClientSecret != "" {
		t.Fatalf("reviewer reads app = %d", rec.Code)
	}

	// The owner can't be removed; reviewers can.
	if rec := e.do(http.MethodDelete, members+"/owner", owner, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("remove owner = %d", rec.Code)
	}
	if rec := e.do(http.MethodDelete, members+"/rita", owner, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove rita = %d", rec.Code)
	}
	if rec := e.do(http.MethodDelete, members+"/rita", owner, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("remove rita again = %d", rec.Code)
	}
	if rec := e.do(http.MethodGet, members, rita, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("removed reviewer reads members = %d", rec.Code)
	}
	got := actions(t, e.st, owner)
	if strings.Join(got, ",") != "app.create,app.member_add,app.member_remove" {
		t.Fatalf("audit = %v", got)
	}
}

func TestDeletedAppStops(t *testing.T) {
	e := newEnv(t)
	owner, alice := e.user("owner"), e.user("alice")
	app := e.createApp(owner, "stardance")
	e.do(http.MethodPatch, "/api/v1/me/apps/"+app.ID, owner, `{"public":true}`)
	tok := e.mustExchange(app, e.codeFor(alice, authParams(app, "profile read")), "")
	pendingCode := e.codeFor(alice, authParams(app, "profile"))
	bob := e.user("bob")
	pendingReq := consentID(t, e.authorize(bob, authParams(app, "profile")))
	cc := decode[tokenJSON](t, e.post("/oauth/token", url.Values{"grant_type": {"client_credentials"}}, creds(app)))

	if rec := e.do(http.MethodDelete, "/api/v1/me/apps/"+app.ID, owner, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := e.probe("/probe/profile", tok.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Errorf("user token after delete = %d", rec.Code)
	}
	if rec := e.probe("/probe/submissions", cc.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Errorf("client token after delete = %d", rec.Code)
	}
	expectOAuthError(t, e.exchange(app, pendingCode, ""), http.StatusUnauthorized, "invalid_client")
	expectOAuthError(t, e.post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}, creds(app)),
		http.StatusUnauthorized, "invalid_client")
	if loc := e.authorize(alice, authParams(app, "profile")); loc.Query().Get("error") != "invalid_client" {
		t.Errorf("authorize after delete: %s", loc)
	}
	if rec := e.do(http.MethodGet, "/api/v1/oauth/requests/"+pendingReq, bob, ""); rec.Code != http.StatusNotFound {
		t.Errorf("pending request after delete = %d", rec.Code)
	}
	if gs := decode[map[string][]oauth.Grant](t, e.do(http.MethodGet, "/api/v1/me/grants", alice, ""))["grants"]; len(gs) != 0 {
		t.Errorf("grants after delete = %+v", gs)
	}
	if ps := decode[map[string][]oauth.Program](t, e.do(http.MethodGet, "/api/v1/programs", [16]byte{}, ""))["programs"]; len(ps) != 0 {
		t.Errorf("programs after delete = %+v", ps)
	}
	if rec := e.do(http.MethodGet, "/api/v1/me/apps/"+app.ID, owner, ""); rec.Code != http.StatusNotFound {
		t.Errorf("get deleted app = %d", rec.Code)
	}
	if got := actions(t, e.st, owner); got[len(got)-1] != "app.delete" {
		t.Errorf("audit = %v", got)
	}
}

func TestPrograms(t *testing.T) {
	e := newEnv(t)
	owner := e.user("owner")
	e.createAppWith(owner, map[string]any{"name": "Zeta", "slug": "zeta", "redirect_uris": []string{callback}, "public": true,
		"homepage_url": "https://zeta.test"})
	e.createAppWith(owner, map[string]any{"name": "alpha", "slug": "alpha", "redirect_uris": []string{callback}, "public": true})
	e.createApp(owner, "private")
	rec := e.do(http.MethodGet, "/api/v1/programs", [16]byte{}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("programs = %d", rec.Code)
	}
	ps := decode[map[string][]oauth.Program](t, rec)["programs"]
	if len(ps) != 2 || ps[0].Slug != "alpha" || ps[1].Slug != "zeta" || ps[1].HomepageURL != "https://zeta.test" {
		t.Fatalf("programs = %+v", ps)
	}
	if strings.Contains(rec.Body.String(), "client") || strings.Contains(rec.Body.String(), "owner") {
		t.Fatalf("program list leaks fields: %s", rec.Body)
	}
}

func TestSessionRoutesNeedASession(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/me/apps"},
		{http.MethodPost, "/api/v1/me/apps"},
		{http.MethodGet, "/api/v1/me/grants"},
		{http.MethodGet, "/api/v1/oauth/requests/x"},
	} {
		if rec := e.do(c.method, c.path, [16]byte{}, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d", c.method, c.path, rec.Code)
		}
	}
}
