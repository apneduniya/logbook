package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/auth"
	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

const (
	testBaseURL = "https://logbook.test"
	testTTL     = time.Hour
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
	svc   *auth.Service
	hca   *fakeHCA
	mux   *http.ServeMux
	clock *clock
}

type envOpt func(*auth.Options)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	st := storetest.New(t)
	hca := newFakeHCA(t, testBaseURL+"/auth/callback")
	clk := &clock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	o := auth.Options{
		BaseURL:    testBaseURL,
		HCA:        auth.HCAConfig{BaseURL: hca.URL, ClientID: hca.clientID, ClientSecret: hca.clientSecret},
		SecretKey:  []byte(strings.Repeat("k", 32)),
		SessionTTL: testTTL,
		HTTPClient: hca.Client(),
		Now:        clk.Now,
	}
	for _, f := range opts {
		f(&o)
	}
	svc, err := auth.New(st, o)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	mux := http.NewServeMux()
	auth.NewHandler(svc).Register(mux)
	// A protected API-key route and a public probe for the middleware tests.
	mux.Handle("GET /api/v1/probe/key", svc.RequireAPIKey(whoami()))
	mux.Handle("POST /api/v1/probe/key", svc.RequireAPIKey(whoami()))
	mux.Handle("POST /api/v1/probe/optional", svc.OptionalSession(whoami()))
	return &env{t: t, st: st, svc: svc, hca: hca, mux: mux, clock: clk}
}

func whoami() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := httpapi.UserFrom(r.Context())
		if !ok {
			httpapi.WriteJSON(w, http.StatusOK, map[string]string{"user": ""})
			return
		}
		httpapi.WriteJSON(w, http.StatusOK, map[string]string{"user": u.Username, "via": u.Via, "id": u.ID.String()})
	})
}

type req struct {
	method, target, body string
	cookies              []*http.Cookie
	header               map[string]string
	remoteAddr           string
}

func (e *env) do(r req) *httptest.ResponseRecorder {
	e.t.Helper()
	if r.method == "" {
		r.method = http.MethodGet
	}
	hr := httptest.NewRequestWithContext(context.Background(), r.method, r.target, strings.NewReader(r.body))
	if r.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	for _, c := range r.cookies {
		hr.AddCookie(c)
	}
	if r.remoteAddr != "" {
		hr.RemoteAddr = r.remoteAddr
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, hr)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// browser is a signed-in session: its cookie and CSRF token.
type browser struct {
	cookie *http.Cookie
	csrf   string
	userID string
	name   string
}

func (b browser) get(e *env, target string) *httptest.ResponseRecorder {
	return e.do(req{target: target, cookies: []*http.Cookie{b.cookie}})
}

func (b browser) send(e *env, method, target, body string) *httptest.ResponseRecorder {
	return e.do(req{method: method, target: target, body: body, cookies: []*http.Cookie{b.cookie},
		header: map[string]string{auth.CSRFHeader: b.csrf}})
}

type authorizeParams struct {
	location  *url.URL
	state     string
	challenge string
	cookie    *http.Cookie
}

// startLogin hits /auth/login and returns where it sent the browser.
func (e *env) startLogin(next string) authorizeParams {
	e.t.Helper()
	target := "/auth/login"
	if next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	rec := e.do(req{target: target})
	if rec.Code != http.StatusFound {
		e.t.Fatalf("login status = %d, body %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatalf("parsing location: %v", err)
	}
	c := cookieNamed(rec, "logbook_oauth")
	if c == nil {
		e.t.Fatal("no state cookie set")
	}
	q := loc.Query()
	return authorizeParams{location: loc, state: q.Get("state"), challenge: q.Get("code_challenge"), cookie: c}
}

// login runs the whole HCA flow and returns the signed-in browser.
func (e *env) login(id fakeIdentity) browser {
	e.t.Helper()
	ap := e.startLogin("")
	code := e.hca.issueCode(ap.challenge, id)
	rec := e.do(req{
		target:  "/auth/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(ap.state),
		cookies: []*http.Cookie{ap.cookie},
	})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		e.t.Fatalf("callback = %d %q, body %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	c := cookieNamed(rec, auth.SessionCookie)
	if c == nil {
		e.t.Fatal("no session cookie after callback")
	}
	return e.sessionFor(c)
}

func (e *env) sessionFor(c *http.Cookie) browser {
	e.t.Helper()
	rec := e.do(req{target: "/api/v1/session", cookies: []*http.Cookie{c}})
	if rec.Code != http.StatusOK {
		e.t.Fatalf("session status = %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	decode(e.t, rec, &body)
	return browser{cookie: c, csrf: body.CSRFToken, userID: body.User.ID, name: body.User.Username}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("counting: %v", err)
	}
	return n
}
