package router_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/auth"
	"github.com/apneduniya/logbook/internal/server/credited"
	"github.com/apneduniya/logbook/internal/server/devices"
	"github.com/apneduniya/logbook/internal/server/hackatime"
	"github.com/apneduniya/logbook/internal/server/ingest"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/live"
	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/profiles"
	"github.com/apneduniya/logbook/internal/server/programapi"
	"github.com/apneduniya/logbook/internal/server/projects"
	"github.com/apneduniya/logbook/internal/server/router"
	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/stats"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/internal/server/submissions"
)

const (
	webOrigin = "https://web.test"
	callback  = "https://program.test/auth/hackatime/callback"
)

// now is the OAuth provider's and the program API's clock. Seeded activity
// is on 10 June 2026, inside every default range.
var now = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// server is the real API handler (router.New) over a fresh database.
type server struct {
	t   *testing.T
	st  *store.Store
	h   http.Handler
	box *secretbox.Box
}

func newServer(t *testing.T) *server {
	t.Helper()
	st := storetest.New(t)
	key := bytes.Repeat([]byte("k"), 32)
	authSvc, err := auth.New(st, auth.Options{
		BaseURL: "http://127.0.0.1:8080", WebURL: webOrigin, SecretKey: key,
		InsecureCookies: true, DevLogin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	htClient, err := hackatime.NewClient(hackatime.ClientConfig{RedirectURL: "http://127.0.0.1:8080/auth/hackatime/callback"})
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return now }
	hub := live.NewHub(live.DefaultLimits())
	t.Cleanup(hub.Close)
	statsSvc := stats.NewService(st)
	profileSvc := profiles.NewService(st, hub)
	oauthSvc := oauth.NewService(st, oauth.Options{WebURL: webOrigin, Now: clock})
	h := router.New(router.Services{
		Ping:       st.Pool.Ping,
		Auth:       authSvc,
		OAuth:      oauthSvc,
		Stats:      statsSvc,
		Profiles:   profileSvc,
		Projects:   projects.NewService(st, profileSvc),
		Hub:        hub,
		Ingest:     ingest.New(st, live.Fanout{statsSvc, hub}),
		Devices:    devices.NewService(st),
		Credited:   credited.NewService(st, hub),
		Ledger:     ledger.NewService(st),
		Hackatime:  hackatime.NewService(st, box, htClient, nil, hackatime.ServiceOptions{}),
		ProgramAPI: programapi.NewService(st, statsSvc, clock),
		// Submissions are only read here; nothing is queued.
		Submissions: submissions.NewService(st, box, submissions.Options{ReviewBaseURL: webOrigin, Members: oauthSvc, Now: clock}),
	}, router.Options{WebOrigin: webOrigin})
	return &server{t: t, st: st, h: h, box: box}
}

// request describes one call. Only the credentials that are set are sent.
type request struct {
	method, target, body string
	browser              *browser // session cookie, plus X-CSRF-Token on writes
	bearer               string
	basic                *[2]string // HTTP Basic client id and secret, form-encoded
	authorization        string     // a raw Authorization header
	form                 url.Values
	origin               string
}

func (s *server) do(rq request) *httptest.ResponseRecorder {
	s.t.Helper()
	if rq.method == "" {
		rq.method = http.MethodGet
	}
	var body *strings.Reader
	switch {
	case rq.form != nil:
		body = strings.NewReader(rq.form.Encode())
	default:
		body = strings.NewReader(rq.body)
	}
	r := httptest.NewRequestWithContext(context.Background(), rq.method, rq.target, body)
	r.RemoteAddr = "127.0.0.1:40000" // dev sign-in only answers loopback peers
	switch {
	case rq.form != nil:
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case rq.body != "":
		r.Header.Set("Content-Type", "application/json")
	}
	if rq.browser != nil {
		r.AddCookie(rq.browser.cookie)
		if rq.method != http.MethodGet {
			r.Header.Set(auth.CSRFHeader, rq.browser.csrf)
		}
	}
	if rq.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+rq.bearer)
	}
	if rq.basic != nil {
		r.SetBasicAuth(url.QueryEscape(rq.basic[0]), url.QueryEscape(rq.basic[1]))
	}
	if rq.authorization != "" {
		r.Header.Set("Authorization", rq.authorization)
	}
	if rq.origin != "" {
		r.Header.Set("Origin", rq.origin)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec
}

// expect calls rq and fails unless the status is want; it returns the body.
func (s *server) expect(rq request, want int) []byte {
	s.t.Helper()
	rec := s.do(rq)
	if rec.Code != want {
		s.t.Fatalf("%s %s = %d %s, want %d", rq.method, rq.target, rec.Code, rec.Body, want)
	}
	return rec.Body.Bytes()
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decoding %q: %v", b, err)
	}
	return v
}

// browser is a signed-in web UI session.
type browser struct {
	id     uuid.UUID
	cookie *http.Cookie
	csrf   string
}

// signIn signs username in through POST /auth/dev-login.
func (s *server) signIn(username string) *browser {
	s.t.Helper()
	rec := s.do(request{method: http.MethodPost, target: "/auth/dev-login", body: `{"username": "` + username + `"}`})
	if rec.Code != http.StatusOK {
		s.t.Fatalf("dev login %s = %d %s", username, rec.Code, rec.Body)
	}
	var out struct {
		User struct {
			ID uuid.UUID `json:"id"`
		} `json:"user"`
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		s.t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			return &browser{id: out.User.ID, cookie: c, csrf: out.CSRF}
		}
	}
	s.t.Fatalf("dev login set no session cookie")
	return nil
}

type app struct {
	ID           string `json:"id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func (a app) creds() *[2]string { return &[2]string{a.ClientID, a.ClientSecret} }

// registerApp registers a confidential app as owner.
func (s *server) registerApp(owner *browser, slug string) app {
	s.t.Helper()
	body := `{"name": "Program ` + slug + `", "slug": "` + slug + `", "redirect_uris": ["` + callback + `"], "homepage_url": "https://program.test"}`
	return decode[app](s.t, s.expect(request{method: http.MethodPost, target: "/api/v1/me/apps", browser: owner, body: body}, http.StatusCreated))
}

// authorize runs GET /oauth/authorize and the consent page's calls for
// user, approving, and returns the code.
func (s *server) authorize(user *browser, a app, scope string) string {
	s.t.Helper()
	q := url.Values{
		"client_id": {a.ClientID}, "redirect_uri": {callback}, "response_type": {"code"},
		"scope": {scope}, "state": {"st8"},
	}
	rec := s.do(request{target: "/oauth/authorize?" + q.Encode(), browser: user})
	if rec.Code != http.StatusFound {
		s.t.Fatalf("authorize = %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		s.t.Fatal(err)
	}
	if loc.Scheme+"://"+loc.Host+loc.Path != webOrigin+"/oauth/authorize" || loc.Query().Get("request") == "" {
		s.t.Fatalf("authorize sent the browser to %s, want the consent page", loc)
	}
	reqPath := "/api/v1/oauth/requests/" + loc.Query().Get("request")
	s.expect(request{target: reqPath, browser: user}, http.StatusOK)
	out := decode[map[string]string](s.t, s.expect(request{method: http.MethodPost, target: reqPath, browser: user, body: `{"approve": true}`}, http.StatusOK))
	back, err := url.Parse(out["redirect"])
	if err != nil {
		s.t.Fatal(err)
	}
	if back.Scheme+"://"+back.Host+back.Path != callback || back.Query().Get("state") != "st8" || back.Query().Get("code") == "" {
		s.t.Fatalf("consent redirect = %s", back)
	}
	return back.Query().Get("code")
}

type token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// exchange trades a code for tokens at POST /oauth/token.
func (s *server) exchange(a app, code string) token {
	s.t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback}}
	return decode[token](s.t, s.expect(request{method: http.MethodPost, target: "/oauth/token", form: form, basic: a.creds()}, http.StatusOK))
}

// seedActivity gives user 30 minutes at the keyboard on project logbook
// (heartbeats behind them) and 20 counted minutes from one device.
func (s *server) seedActivity(user uuid.UUID) {
	s.t.Helper()
	at := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	seedtest.Span(s.t, s.st, user, at, 30, seedtest.Minute{Project: "logbook", Language: "Go"})
	dev := seedtest.Device(s.t, s.st, user, "laptop")
	seedtest.CreditedSpan(s.t, s.st, user, dev, at, 20, seedtest.Credited{Project: "logbook", Seconds: 60})
	var hbs []seedtest.Heartbeat
	for i := range 30 {
		hbs = append(hbs, seedtest.Heartbeat{At: at.Add(time.Duration(i) * time.Minute), Project: "logbook"})
	}
	seedtest.Heartbeats(s.t, s.st, user, hbs...)
}

const (
	countedSeconds = 20 * 60
	rawSeconds     = 30 * 60
)

func asInt(t *testing.T, v any) int64 {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("%v (%T) is not a JSON number", v, v)
	}
	i, err := n.Int64()
	if err != nil {
		t.Fatalf("%v is not an integer: %v", v, err)
	}
	return i
}

// TestProgramFlowEndToEnd runs a program's whole journey on the real
// handler: user A registers an app, user B approves it, the app exchanges
// the code and reads B's data with the token, B revokes the grant.
func TestProgramFlowEndToEnd(t *testing.T) {
	s := newServer(t)
	owner, orpheus := s.signIn("fiona"), s.signIn("orpheus")
	s.seedActivity(orpheus.id)
	orpheusID := seedtest.PublicID(t, s.st, orpheus.id)
	stardance := s.registerApp(owner, "stardance")
	if stardance.ClientSecret == "" {
		t.Fatal("no client secret on a confidential app")
	}

	// B grants profile and read; the program exchanges the code.
	tok := s.exchange(stardance, s.authorize(orpheus, stardance, "profile read"))
	if tok.Scope != "profile read" || !strings.HasPrefix(tok.AccessToken, "lbo_") || tok.RefreshToken == "" {
		t.Fatalf("token = %+v", tok)
	}

	t.Run("authenticated/me has the numeric public id", func(t *testing.T) {
		me := decode[map[string]any](t, s.expect(request{target: "/api/v1/authenticated/me", bearer: tok.AccessToken}, http.StatusOK))
		if got := asInt(t, me["id"]); got != orpheusID {
			t.Fatalf("id = %d, want public id %d", got, orpheusID)
		}
		if lb, _ := me["logbook"].(map[string]any); lb["username"] != "orpheus" {
			t.Fatalf("logbook = %v", me["logbook"])
		}
	})

	t.Run("authenticated/projects reports counted time", func(t *testing.T) {
		out := decode[struct {
			Projects []map[string]any `json:"projects"`
		}](t, s.expect(request{target: "/api/v1/authenticated/projects", bearer: tok.AccessToken}, http.StatusOK))
		if len(out.Projects) != 1 || out.Projects[0]["name"] != "logbook" {
			t.Fatalf("projects = %v", out.Projects)
		}
		p := out.Projects[0]
		if asInt(t, p["total_seconds"]) != countedSeconds || asInt(t, p["credited_seconds"]) != countedSeconds ||
			asInt(t, p["raw_seconds"]) != rawSeconds {
			t.Fatalf("project = %v", p)
		}
	})

	t.Run("users/my/stats: counted by default, raw with raw=true", func(t *testing.T) {
		for _, tc := range []struct {
			query string
			want  int64
		}{
			{"total_seconds=true", countedSeconds},
			{"total_seconds=true&raw=true", rawSeconds},
		} {
			got := decode[map[string]any](t, s.expect(request{target: "/api/v1/users/my/stats?" + tc.query, bearer: tok.AccessToken}, http.StatusOK))
			if asInt(t, got["total_seconds"]) != tc.want || asInt(t, got["credited_seconds"]) != countedSeconds ||
				asInt(t, got["raw_seconds"]) != rawSeconds {
				t.Errorf("%s = %v, want total %d", tc.query, got, tc.want)
			}
		}
		body := decode[map[string]map[string]any](t, s.expect(request{target: "/api/v1/users/my/stats", bearer: tok.AccessToken}, http.StatusOK))
		if body["data"]["user_id"] != strconv.FormatInt(orpheusID, 10) || asInt(t, body["data"]["total_seconds"]) != countedSeconds {
			t.Fatalf("stats data = %v", body["data"])
		}
		// The numeric id names the same user.
		byID := decode[map[string]any](t, s.expect(request{target: "/api/v1/users/" + strconv.FormatInt(orpheusID, 10) + "/stats?total_seconds=true"}, http.StatusOK))
		if asInt(t, byID["total_seconds"]) != countedSeconds {
			t.Fatalf("by public id = %v", byID)
		}
	})

	// B makes the profile private and grants a second app profile only.
	s.expect(request{method: http.MethodPatch, target: "/api/v1/me/profile", browser: orpheus, body: `{"profile_public": false}`}, http.StatusOK)
	other := s.registerApp(owner, "other-program")
	profileOnly := s.exchange(other, s.authorize(orpheus, other, "profile"))
	ownerTok := s.exchange(stardance, s.authorize(owner, stardance, "profile read"))
	statsPath := "/api/v1/users/" + strconv.FormatInt(orpheusID, 10) + "/stats"

	t.Run("a private profile needs a read token from its user", func(t *testing.T) {
		s.expect(request{target: statsPath}, http.StatusForbidden)
		s.expect(request{target: statsPath, bearer: tok.AccessToken}, http.StatusOK)
		s.expect(request{target: "/api/v1/users/orpheus/projects", bearer: tok.AccessToken}, http.StatusOK)
		// B's own token for another app, without read.
		s.expect(request{target: statsPath, bearer: profileOnly.AccessToken}, http.StatusForbidden)
		s.expect(request{target: "/api/v1/users/orpheus/projects", bearer: profileOnly.AccessToken}, http.StatusForbidden)
		s.expect(request{target: "/api/v1/authenticated/projects", bearer: profileOnly.AccessToken}, http.StatusForbidden)
		s.expect(request{target: "/api/v1/authenticated/me", bearer: profileOnly.AccessToken}, http.StatusOK)
		// Another user's read token for the same app.
		s.expect(request{target: statsPath, bearer: ownerTok.AccessToken}, http.StatusForbidden)
		me := decode[map[string]any](t, s.expect(request{target: "/api/v1/users/my/stats?total_seconds=true", bearer: ownerTok.AccessToken}, http.StatusOK))
		if asInt(t, me["total_seconds"]) != 0 {
			t.Fatalf("owner's my/stats read orpheus's time: %v", me)
		}
	})

	t.Run("another user and the app owner can't see B's grants or requests", func(t *testing.T) {
		grants := decode[struct {
			Grants []map[string]any `json:"grants"`
		}](t, s.expect(request{target: "/api/v1/me/grants", browser: owner}, http.StatusOK))
		for _, g := range grants.Grants {
			if g["app_id"] == other.ID {
				t.Fatalf("owner sees a grant they never gave: %v", g)
			}
		}
		s.expect(request{method: http.MethodDelete, target: "/api/v1/me/grants/" + other.ID, browser: owner}, http.StatusNotFound)
		s.expect(request{target: "/api/v1/authenticated/me", bearer: profileOnly.AccessToken}, http.StatusOK)
	})

	t.Run("revoking the grant kills the token", func(t *testing.T) {
		s.expect(request{method: http.MethodDelete, target: "/api/v1/me/grants/" + stardance.ID, browser: orpheus}, http.StatusNoContent)
		rec := s.do(request{target: "/api/v1/authenticated/me", bearer: tok.AccessToken})
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
			t.Fatalf("revoked token on authenticated/me = %d %v", rec.Code, rec.Header())
		}
		s.expect(request{target: statsPath, bearer: tok.AccessToken}, http.StatusForbidden)
		s.expect(request{target: "/api/v1/users/my/stats", bearer: tok.AccessToken}, http.StatusNotFound)
		s.expect(request{target: "/api/v1/my/heartbeats", bearer: tok.AccessToken}, http.StatusUnauthorized)
		refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}
		s.expect(request{method: http.MethodPost, target: "/oauth/token", form: refresh, basic: stardance.creds()}, http.StatusBadRequest)
		// The grant B gave the other app is untouched, and so is the
		// owner's own grant to this app.
		s.expect(request{target: "/api/v1/authenticated/me", bearer: profileOnly.AccessToken}, http.StatusOK)
		s.expect(request{target: "/api/v1/authenticated/me", bearer: ownerTok.AccessToken}, http.StatusOK)
	})
}

// TestClientCredentialsReadNoUserData: a program's own token acts as the
// app, so no program API call reads a user with it.
func TestClientCredentialsReadNoUserData(t *testing.T) {
	s := newServer(t)
	owner, orpheus := s.signIn("fiona"), s.signIn("orpheus")
	s.seedActivity(orpheus.id)
	a := s.registerApp(owner, "stardance")

	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"read"}}
	s.expect(request{method: http.MethodPost, target: "/oauth/token", form: form, basic: a.creds()}, http.StatusBadRequest)
	form.Set("scope", "submissions")
	tok := decode[token](t, s.expect(request{method: http.MethodPost, target: "/oauth/token", form: form, basic: a.creds()}, http.StatusOK))
	if tok.AccessToken == "" || tok.RefreshToken != "" {
		t.Fatalf("client token = %+v", tok)
	}

	s.expect(request{method: http.MethodPatch, target: "/api/v1/me/profile", browser: orpheus, body: `{"profile_public": false}`}, http.StatusOK)
	for _, tc := range []struct {
		target string
		want   int
	}{
		{"/api/v1/authenticated/me", http.StatusUnauthorized},
		{"/api/v1/authenticated/projects", http.StatusUnauthorized},
		{"/api/v1/users/my/stats", http.StatusNotFound},
		{"/api/v1/users/orpheus/stats", http.StatusForbidden},
		{"/api/v1/users/orpheus/projects", http.StatusForbidden},
		{"/api/v1/users/orpheus/heartbeats/spans", http.StatusForbidden},
		{"/api/v1/my/heartbeats", http.StatusUnauthorized},
	} {
		if rec := s.do(request{target: tc.target, bearer: tok.AccessToken}); rec.Code != tc.want {
			t.Errorf("client token on %s = %d %s, want %d", tc.target, rec.Code, rec.Body, tc.want)
		}
	}
}

// TestCredentialsWorkOnlyWhereDocumented pins, on the real routes, which
// credential each family of endpoints takes (auth.md, programs.md).
func TestCredentialsWorkOnlyWhereDocumented(t *testing.T) {
	s := newServer(t)
	owner, orpheus := s.signIn("fiona"), s.signIn("orpheus")
	s.seedActivity(orpheus.id)
	a := s.registerApp(owner, "stardance")
	tok := s.exchange(a, s.authorize(orpheus, a, "profile read"))
	key := decode[map[string]any](t, s.expect(request{method: http.MethodPost, target: "/api/v1/api_keys", browser: orpheus, body: `{"name": "laptop"}`}, http.StatusCreated))["key"].(string)
	s.expect(request{method: http.MethodPatch, target: "/api/v1/me/profile", browser: orpheus, body: `{"profile_public": false}`}, http.StatusOK)

	for _, tc := range []struct {
		name string
		rq   request
		want int
	}{
		// authenticated/*: OAuth only.
		{"token on authenticated/me", request{target: "/api/v1/authenticated/me", bearer: tok.AccessToken}, http.StatusOK},
		{"API key on authenticated/me", request{target: "/api/v1/authenticated/me", bearer: key}, http.StatusUnauthorized},
		{"session on authenticated/me", request{target: "/api/v1/authenticated/me", browser: orpheus}, http.StatusUnauthorized},
		// users/{id}/* and my/heartbeats: a read token or the user's key.
		{"API key on users/my/stats", request{target: "/api/v1/users/my/stats", bearer: key}, http.StatusOK},
		{"API key on a private profile's stats", request{target: "/api/v1/users/orpheus/stats", bearer: key}, http.StatusOK},
		{"API key as Basic on my/heartbeats", request{target: "/api/v1/my/heartbeats", authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte(key))}, http.StatusOK},
		{"API key as Basic on users/my/stats", request{target: "/api/v1/users/my/stats", authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte(key))}, http.StatusNotFound},
		{"token on my/heartbeats", request{target: "/api/v1/my/heartbeats", bearer: tok.AccessToken}, http.StatusOK},
		{"session on users/my/stats", request{target: "/api/v1/users/my/stats", browser: orpheus}, http.StatusNotFound},
		{"session on a private profile's stats", request{target: "/api/v1/users/orpheus/stats", browser: orpheus}, http.StatusForbidden},
		// Web UI and agent routes never take an OAuth token.
		{"token on me/summary", request{target: "/api/v1/me/summary", bearer: tok.AccessToken}, http.StatusUnauthorized},
		{"token on me/grants", request{target: "/api/v1/me/grants", bearer: tok.AccessToken}, http.StatusUnauthorized},
		{"token on statusbar", request{target: "/api/v1/users/current/statusbar/today", bearer: tok.AccessToken}, http.StatusUnauthorized},
		{"API key on statusbar", request{target: "/api/v1/users/current/statusbar/today", bearer: key}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := s.do(tc.rq); rec.Code != tc.want {
				t.Fatalf("= %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
}

// TestCORSCoversOnlyBrowserRoutes: the web origin gets CORS on the web UI's
// routes (including the consent and grant calls) but not on the program
// API or the OAuth endpoints, which only servers call.
func TestCORSCoversOnlyBrowserRoutes(t *testing.T) {
	s := newServer(t)
	orpheus := s.signIn("orpheus")
	for _, tc := range []struct {
		target string
		cors   bool
	}{
		{"/api/v1/me/grants", true},
		{"/api/v1/me/apps", true},
		{"/api/v1/programs", true},
		{"/api/v1/users/orpheus/profile", true},
		{"/api/v1/authenticated/me", false},
		{"/api/v1/authenticated/projects", false},
		{"/api/v1/users/orpheus/stats", false},
		{"/api/v1/users/orpheus/trust_factor", false},
		{"/api/v1/users/orpheus/heartbeats/spans", false},
		{"/api/v1/users/orpheus/projects", false},
		{"/api/v1/users/orpheus/project/logbook", false},
		{"/api/v1/users/orpheus/projects/details", false},
		{"/api/v1/my/heartbeats", false},
		{"/oauth/authorize", false},
		{"/api/v1/me/submissions", true},
		{"/api/v1/review/AAAAAAAAAAAAAAAAAAAAAA", true},
		{"/api/v1/me/submissions/plan", false},
		{"/api/v1/submissions", false},
	} {
		rec := s.do(request{target: tc.target, browser: orpheus, origin: webOrigin})
		if got := rec.Header().Get("Access-Control-Allow-Origin") == webOrigin; got != tc.cors {
			t.Errorf("%s: CORS = %v, want %v", tc.target, got, tc.cors)
		}
	}
	for _, path := range []string{"/oauth/token", "/oauth/revoke"} {
		rec := s.do(request{method: http.MethodPost, target: path, form: url.Values{}, origin: webOrigin})
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s answered CORS", path)
		}
	}
}
