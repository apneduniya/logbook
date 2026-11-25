package oauth_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

const (
	webURL      = "https://web.test"
	callback    = "https://program.test/callback"
	devCallback = "http://localhost:3000/cb"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	t     *testing.T
	st    *store.Store
	svc   *oauth.Service
	mux   *http.ServeMux
	clock *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := storetest.New(t)
	clk := &clock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	svc := oauth.NewService(st, oauth.Options{WebURL: webURL, Now: clk.Now})
	mux := http.NewServeMux()
	oauth.NewHandler(svc, httpapitest.StubSession, optionalSession, httpapitest.NoLimit, oauth.HandlerOptions{}).Register(mux)
	// Probes for the bearer middleware.
	mux.Handle("GET /probe/profile", svc.RequireScope(oauth.ScopeProfile)(whoami()))
	mux.Handle("GET /probe/read", svc.RequireScope(oauth.ScopeRead)(whoami()))
	mux.Handle("GET /probe/submissions", svc.RequireScope(oauth.ScopeSubmissions)(whoami()))
	mux.Handle("GET /probe/optional", svc.OptionalScope(oauth.ScopeRead)(whoami()))
	return &env{t: t, st: st, svc: svc, mux: mux, clock: clk}
}

// optionalSession stands in for auth.Service.OptionalSession: it attaches
// the user named in the test header, if any.
func optionalSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, err := uuid.Parse(r.Header.Get(httpapitest.UserHeader)); err == nil {
			r = r.WithContext(httpapi.WithUser(r.Context(), httpapi.CurrentUser{ID: id, Via: "session"}))
		}
		next.ServeHTTP(w, r)
	})
}

type probeJSON struct {
	User   string   `json:"user"`
	Via    string   `json:"via"`
	App    string   `json:"app"`
	Scopes []string `json:"scopes"`
	Client bool     `json:"client"`
}

func whoami() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out probeJSON
		if u, ok := httpapi.UserFrom(r.Context()); ok {
			out.User, out.Via = u.ID.String(), u.Via
		}
		if tok, ok := httpapi.OAuthTokenFrom(r.Context()); ok {
			out.App, out.Scopes, out.Client = tok.AppID.String(), tok.Scopes, tok.IsClient()
		}
		httpapi.WriteJSON(w, http.StatusOK, out)
	})
}

func (e *env) do(method, target string, user uuid.UUID, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	r := httpapitest.Request(e.t, method, target, user, body)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	e.mux.ServeHTTP(rec, r)
	return rec
}

func (e *env) user(name string) uuid.UUID {
	e.t.Helper()
	return seedtest.User(e.t, e.st, name, "UTC", true)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

type appJSON struct {
	ID           string   `json:"id"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Name         string   `json:"name"`
	Slug         string   `json:"slug"`
	RedirectURIs []string `json:"redirect_uris"`
	Public       bool     `json:"public"`
	Confidential bool     `json:"confidential"`
	Role         string   `json:"role"`
}

// createApp registers a confidential app with the callback URIs.
func (e *env) createApp(owner uuid.UUID, slug string) appJSON {
	e.t.Helper()
	return e.createAppWith(owner, map[string]any{
		"name": "App " + slug, "slug": slug, "redirect_uris": []string{callback, devCallback},
		"homepage_url": "https://program.test",
	})
}

func (e *env) createAppWith(owner uuid.UUID, body map[string]any) appJSON {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/v1/me/apps", owner, mustJSON(body))
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("create app = %d %s", rec.Code, rec.Body)
	}
	return decode[appJSON](e.t, rec)
}

// authorize calls GET /oauth/authorize and returns the Location it redirects to.
func (e *env) authorize(user uuid.UUID, q url.Values) *url.URL {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), user, "")
	if rec.Code != http.StatusFound {
		e.t.Fatalf("authorize = %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatalf("location: %v", err)
	}
	return loc
}

func authParams(app appJSON, scope string) url.Values {
	return url.Values{
		"client_id": {app.ClientID}, "redirect_uri": {callback}, "response_type": {"code"},
		"scope": {scope}, "state": {"xyz"},
	}
}

// consentID asserts loc is the web consent page and returns the request id.
func consentID(t *testing.T, loc *url.URL) string {
	t.Helper()
	if loc.Scheme+"://"+loc.Host != webURL || loc.Path != "/oauth/authorize" || loc.Query().Get("request") == "" {
		t.Fatalf("redirect = %s, want the consent page", loc)
	}
	return loc.Query().Get("request")
}

// decide posts the consent decision and returns the redirect.
func (e *env) decide(user uuid.UUID, id string, approve bool) *url.URL {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/v1/oauth/requests/"+id, user, mustJSON(map[string]bool{"approve": approve}))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("decide = %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(decode[map[string]string](e.t, rec)["redirect"])
	if err != nil {
		e.t.Fatal(err)
	}
	return loc
}

// codeFor runs authorize and consent for user and returns the code.
func (e *env) codeFor(user uuid.UUID, q url.Values) string {
	e.t.Helper()
	loc := e.authorize(user, q)
	if loc.Query().Get("code") == "" {
		loc = e.decide(user, consentID(e.t, loc), true)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != q.Get("redirect_uri") {
		e.t.Fatalf("redirected to %s, want %s", loc, q.Get("redirect_uri"))
	}
	if loc.Query().Get("state") != q.Get("state") {
		e.t.Fatalf("state = %q, want %q", loc.Query().Get("state"), q.Get("state"))
	}
	code := loc.Query().Get("code")
	if code == "" {
		e.t.Fatalf("no code in %s", loc)
	}
	return code
}

// tokenReq posts form to path with HTTP Basic client authentication when
// basic is non-nil.
func (e *env) post(path string, form url.Values, basic *[2]string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic != nil {
		r.SetBasicAuth(url.QueryEscape(basic[0]), url.QueryEscape(basic[1]))
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, r)
	return rec
}

func creds(app appJSON) *[2]string { return &[2]string{app.ClientID, app.ClientSecret} }

type tokenJSON struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	CreatedAt    int64  `json:"created_at"`
}

type oauthErrJSON struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (e *env) exchange(app appJSON, code, verifier string) *httptest.ResponseRecorder {
	e.t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback}}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	if app.ClientSecret == "" {
		form.Set("client_id", app.ClientID)
		return e.post("/oauth/token", form, nil)
	}
	return e.post("/oauth/token", form, creds(app))
}

func (e *env) mustExchange(app appJSON, code, verifier string) tokenJSON {
	e.t.Helper()
	rec := e.exchange(app, code, verifier)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("exchange = %d %s", rec.Code, rec.Body)
	}
	return decode[tokenJSON](e.t, rec)
}

// probe calls a bearer-protected probe and returns the status.
func (e *env) probe(path, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, r)
	return rec
}

func expectOAuthError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d %s, want %d %s", rec.Code, rec.Body, status, code)
	}
	if got := decode[oauthErrJSON](t, rec); got.Error != code {
		t.Fatalf("error = %q (%s), want %q", got.Error, got.Description, code)
	}
}

func pkcePair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func actions(t *testing.T, st *store.Store, actor uuid.UUID) []string {
	t.Helper()
	var out []string
	for _, a := range seedtest.Audit(t, st, actor) {
		out = append(out, a.Action)
	}
	return out
}

// gate holds every INSERT into oauth_tokens at a trigger until release, so
// a test can line up a concurrent transaction behind one that is about to
// issue a token.
type gate struct {
	e        *env
	ctx      context.Context
	release  func()
	released bool
}

// gateTokenInserts installs the gate. Call release (also deferred) to let
// the held inserts through.
func (e *env) gateTokenInserts(ctx context.Context) *gate {
	e.t.Helper()
	return e.gateAt(ctx, "BEFORE INSERT ON oauth_tokens")
}

// gateAt installs a gate at a row trigger point such as
// "AFTER UPDATE ON oauth_grants": every statement reaching it waits there
// until release.
func (e *env) gateAt(ctx context.Context, when string) *gate {
	e.t.Helper()
	const lock = 727021
	hold, err := e.st.Pool.Acquire(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(hold.Release)
	if _, err := hold.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		e.t.Fatal(err)
	}
	g := &gate{e: e, ctx: ctx}
	g.release = func() {
		if !g.released {
			g.released = true
			if _, err := hold.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock); err != nil {
				e.t.Errorf("unlocking: %v", err)
			}
		}
	}
	if _, err := e.st.Pool.Exec(ctx, `
		CREATE FUNCTION test_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(727021); RETURN NEW; END $$;
		CREATE TRIGGER test_gate `+when+` FOR EACH ROW EXECUTE FUNCTION test_gate();`); err != nil {
		e.t.Fatal(err)
	}
	return g
}

// waiting returns once n of this database's backends are blocked on a lock.
func (g *gate) waiting(n int) {
	g.e.t.Helper()
	for g.ctx.Err() == nil {
		var got int
		if err := g.e.st.Pool.QueryRow(g.ctx, `SELECT count(*)::int FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			g.e.t.Fatalf("counting blocked backends: %v", err)
		}
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.e.t.Fatalf("timed out waiting for %d blocked backends", n)
}

// refreshAsync posts a refresh in the background and sends its response.
func (e *env) refreshAsync(ctx context.Context, app appJSON, refresh string) <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth(url.QueryEscape(app.ClientID), url.QueryEscape(app.ClientSecret))
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, r)
		out <- rec
	}()
	return out
}
