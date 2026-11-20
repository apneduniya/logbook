package hackatime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

const (
	fakeClientID     = "logbook-test-client"
	fakeClientSecret = "logbook-test-secret"
	fakeRedirect     = "https://logbook.test/auth/hackatime/callback"
)

type fakeUser struct {
	id       int64
	username string
	github   string
	trust    string
}

type fakeGrant struct {
	challenge string
	userID    int64
}

// fakeHackatime is an httptest server speaking the subset of Hackatime's API
// that Logbook uses, with the same shapes as Hackatime's controllers.
type fakeHackatime struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	users      map[int64]*fakeUser
	grants     map[string]fakeGrant // code -> grant
	tokens     map[string]int64     // token -> user id
	revoked    []string
	heartbeats map[int64][]map[string]any // user id -> rendered rows
	userAgents []string
	trustCalls int      // requests to the public trust_factor endpoint
	hbCalls    []string // "start..end" of each heartbeats call that succeeded
	// hbHook, when set, may answer a heartbeats request itself (return true).
	hbHook  func(w http.ResponseWriter, r *http.Request, call int) bool
	hbTotal int
	// meHook, when set, may answer a /me request itself.
	meHook func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeHackatime(t *testing.T) *fakeHackatime {
	t.Helper()
	f := &fakeHackatime{
		t: t, users: map[int64]*fakeUser{}, grants: map[string]fakeGrant{},
		tokens: map[string]int64{}, heartbeats: map[int64][]map[string]any{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", f.token)
	mux.HandleFunc("POST /oauth/revoke", f.revoke)
	mux.HandleFunc("GET /api/v1/authenticated/me", f.me)
	mux.HandleFunc("GET /api/v1/users/{id}/trust_factor", f.trustFactor)
	mux.HandleFunc("GET /api/v1/my/heartbeats", f.myHeartbeats)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.userAgents = append(f.userAgents, r.UserAgent())
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHackatime) addUser(id int64, trust string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[id] = &fakeUser{id: id, github: "gh" + strconv.FormatInt(id, 10), trust: trust}
}

// setMeHook installs (or, with nil, removes) a /me override.
func (f *fakeHackatime) setMeHook(h func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meHook = h
}

// setHBHook installs (or, with nil, removes) a heartbeats override.
func (f *fakeHackatime) setHBHook(h func(w http.ResponseWriter, r *http.Request, call int) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hbHook = h
}

// setUsername sets a Hackatime username. Hackatime allows all-digit names.
func (f *fakeHackatime) setUsername(id int64, username string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[id].username = username
}

// liveToken reports whether Hackatime still accepts token.
func (f *fakeHackatime) liveToken(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tokens[token]
	return ok
}

func (f *fakeHackatime) trustFactorCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trustCalls
}

// issue returns the user's live token, or mints one. Like Hackatime's
// Doorkeeper (reuse_access_token), the same user, app and scopes get the same
// token back until it is revoked. Callers hold f.mu.
func (f *fakeHackatime) issue(hackatimeUserID int64) string {
	for tok, id := range f.tokens {
		if id == hackatimeUserID {
			return tok
		}
	}
	tok := "tok_" + randHex(16)
	f.tokens[tok] = hackatimeUserID
	return tok
}

func (f *fakeHackatime) setTrust(id int64, trust string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[id].trust = trust
}

// authorize plays the user consenting on Hackatime: it validates the
// authorize URL Logbook built and returns the code Hackatime would redirect
// back with, bound to the PKCE challenge in that URL.
func (f *fakeHackatime) authorize(authURL string, hackatimeUserID int64) (code, state string) {
	f.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		f.t.Fatalf("parsing authorize URL: %v", err)
	}
	q := u.Query()
	if u.Path != "/oauth/authorize" || q.Get("client_id") != fakeClientID || q.Get("redirect_uri") != fakeRedirect ||
		q.Get("response_type") != "code" || q.Get("scope") != "profile read" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" {
		f.t.Fatalf("unexpected authorize URL %s", authURL)
	}
	code = randHex(16)
	f.mu.Lock()
	f.grants[code] = fakeGrant{challenge: q.Get("code_challenge"), userID: hackatimeUserID}
	f.mu.Unlock()
	return code, q.Get("state")
}

// tokenFor issues a token directly, for tests that skip the OAuth dance.
func (f *fakeHackatime) tokenFor(hackatimeUserID int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issue(hackatimeUserID)
}

func (f *fakeHackatime) revokeAll(hackatimeUserID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for tok, id := range f.tokens {
		if id == hackatimeUserID {
			delete(f.tokens, tok)
		}
	}
}

func (f *fakeHackatime) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeHackatime) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	if r.PostForm.Get("client_id") != fakeClientID || r.PostForm.Get("client_secret") != fakeClientSecret {
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code := r.PostForm.Get("code")
	g, ok := f.grants[code]
	delete(f.grants, code) // codes are single use
	if r.PostForm.Get("grant_type") != "authorization_code" || !ok ||
		r.PostForm.Get("redirect_uri") != fakeRedirect ||
		oauth2.S256ChallengeFromVerifier(r.PostForm.Get("code_verifier")) != g.challenge {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "The provided authorization grant is invalid"})
		return
	}
	tok := f.issue(g.userID)
	writeJSON(w, 200, map[string]any{
		"access_token": tok, "token_type": "Bearer", "expires_in": 504921600,
		"scope": "profile read", "created_at": time.Now().Unix(),
	})
}

func (f *fakeHackatime) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(400)
		return
	}
	f.mu.Lock()
	tok := r.PostForm.Get("token")
	f.revoked = append(f.revoked, tok)
	delete(f.tokens, tok)
	f.mu.Unlock()
	writeJSON(w, 200, map[string]any{})
}

func (f *fakeHackatime) bearer(r *http.Request) (*fakeUser, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || h[:len(p)] != p {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.tokens[h[len(p):]]
	if !ok {
		return nil, false
	}
	return f.users[id], true
}

var fakeTrustValues = map[string]int{"blue": 0, "red": 1, "green": 2, "yellow": 3}

func (f *fakeHackatime) me(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hook := f.meHook
	f.mu.Unlock()
	if hook != nil && hook(w, r) {
		return
	}
	u, ok := f.bearer(r)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}
	f.mu.Lock()
	trust := u.trust
	f.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"id": u.id, "emails": []string{"someone@example.com"}, "slack_id": "U123",
		"github_username": u.github,
		"trust_factor":    map[string]any{"trust_level": trust, "trust_value": fakeTrustValues[trust]},
	})
}

// trustFactor mirrors StatsController#trust_factor: the identifier matches a
// slack id, username or numeric id, and the first row wins with no ordering.
// The fake resolves the ambiguity the worst way, username first, so a test
// would catch Logbook relying on it.
func (f *fakeHackatime) trustFactor(w http.ResponseWriter, r *http.Request) {
	ident := r.PathValue("id")
	f.mu.Lock()
	f.trustCalls++
	var u *fakeUser
	for _, c := range f.users {
		if c.username == ident {
			u = c
		}
	}
	if id, err := strconv.ParseInt(ident, 10, 64); u == nil && err == nil {
		u = f.users[id]
	}
	ok := u != nil
	var trust string
	if ok {
		trust = u.trust
	}
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "User not found"})
		return
	}
	if trust == "yellow" {
		trust = "blue"
	}
	writeJSON(w, 200, map[string]any{"trust_level": trust, "trust_value": fakeTrustValues[trust]})
}

// addHeartbeat stores a heartbeat rendered the way Rails renders a row.
func (f *fakeHackatime) addHeartbeat(userID int64, at time.Time, entity string, extra map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := map[string]any{
		"id": len(f.heartbeats[userID]) + 1, "user_id": userID,
		"time": float64(at.UnixMicro()) / 1e6, "entity": entity, "type": "file",
		"category": "coding", "project": "logbook", "branch": "main", "language": "Go",
		"dependencies": []string{"fmt"}, "editor": "vscode", "machine": "laptop",
		"operating_system": "linux", "user_agent": "wakatime/v1.0 vscode-wakatime/24.0",
		"is_write": false, "lines": 10, "lineno": 1, "cursorpos": 2,
		"line_additions": nil, "line_deletions": nil, "project_root_count": 3,
		"source_type": "direct_entry", "ip_address": "203.0.113.9", "ja4_id": nil,
		"fields_hash": randHex(16), "created_at": at.Format(time.RFC3339), "updated_at": at.Format(time.RFC3339),
		"deleted_at": nil, "ai_model": nil, "ai_session": nil, "ai_subscription_plan": nil,
		"ai_input_tokens": nil, "ai_output_tokens": nil, "ai_prompt_length": nil,
		"ai_line_changes": nil, "human_line_changes": nil,
	}
	for k, v := range extra {
		row[k] = v
	}
	f.heartbeats[userID] = append(f.heartbeats[userID], row)
}

func (f *fakeHackatime) myHeartbeats(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hbTotal++
	call := f.hbTotal
	hook := f.hbHook
	f.mu.Unlock()
	if hook != nil && hook(w, r, call) {
		return
	}
	u, ok := f.bearer(r)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "Unauthorized"})
		return
	}
	start, err1 := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start_time"))
	end, err2 := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end_time"))
	if err1 != nil || err2 != nil {
		writeJSON(w, 400, map[string]string{"error": "bad time"})
		return
	}
	f.mu.Lock()
	var out []map[string]any
	for _, h := range f.heartbeats[u.id] {
		ts := h["time"].(float64)
		if ts >= float64(start.UnixMicro())/1e6 && ts <= float64(end.UnixMicro())/1e6 {
			out = append(out, h)
		}
	}
	f.hbCalls = append(f.hbCalls, start.Format(time.RFC3339)+".."+end.Format(time.RFC3339))
	f.mu.Unlock()
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{
		"start_time": start, "end_time": end, "total_seconds": 0, "heartbeats": out,
	})
}

func (f *fakeHackatime) heartbeatCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hbCalls...)
}

func (f *fakeHackatime) client(t *testing.T, mod func(*ClientConfig)) *Client {
	t.Helper()
	cfg := ClientConfig{
		BaseURL: f.srv.URL, ClientID: fakeClientID, ClientSecret: fakeClientSecret, RedirectURL: fakeRedirect,
		MinInterval: time.Millisecond, MaxRetryWait: 5 * time.Second, RequestTimeout: 5 * time.Second,
	}
	if mod != nil {
		mod(&cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() err = %v", err)
	}
	return c
}
