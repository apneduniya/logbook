package hackatime

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/pkg/wire"
)

// syncBuffer collects log output from concurrent goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// memSink is an in-memory HeartbeatSink that deduplicates like ingest does.
type memSink struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID]map[string]wire.Heartbeat
	chunks  []int
	sources []string
	// failOn makes the n-th Ingest call (1-based) fail.
	failOn int
	calls  int
}

func newMemSink() *memSink { return &memSink{byUser: map[uuid.UUID]map[string]wire.Heartbeat{}} }

func (m *memSink) Ingest(_ context.Context, userID uuid.UUID, hbs []wire.Heartbeat, source string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == m.failOn {
		return 0, errSinkDown
	}
	m.chunks = append(m.chunks, len(hbs))
	m.sources = append(m.sources, source)
	if m.byUser[userID] == nil {
		m.byUser[userID] = map[string]wire.Heartbeat{}
	}
	n := 0
	for _, h := range hbs {
		k := string(h.FieldsHash())
		if _, dup := m.byUser[userID][k]; !dup {
			m.byUser[userID][k] = h
			n++
		}
	}
	return n, nil
}

func (m *memSink) count(userID uuid.UUID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byUser[userID])
}

type sinkError string

func (e sinkError) Error() string { return string(e) }

const errSinkDown = sinkError("sink down")

type testEnv struct {
	st      *store.Store
	fake    *fakeHackatime
	client  *Client
	box     *secretbox.Box
	svc     *Service
	sink    *memSink
	logs    *syncBuffer
	logger  *slog.Logger
	mux     *http.ServeMux
	now     time.Time
	clockMu sync.Mutex
}

func (e *testEnv) clock() time.Time {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	return e.now
}

func (e *testEnv) advance(d time.Duration) {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	e.now = e.now.Add(d)
}

// newEnv wires a Service and Handler to a fresh database and a fake Hackatime.
// The import window is 2025-03-01 .. 2025-03-03.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{st: storetest.New(t), fake: newFakeHackatime(t), sink: newMemSink(), logs: &syncBuffer{}}
	e.now = time.Date(2025, 3, 3, 15, 0, 0, 0, time.UTC)
	e.logger = slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.client = e.fake.client(t, nil)
	var err error
	e.box, err = secretbox.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	// Insert-only River client: jobs land in river_job inside the link tx.
	rc, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Logger: e.logger})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = NewService(e.st, e.box, e.client, rc, ServiceOptions{
		Logger: e.logger, ImportFrom: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), Now: e.clock,
	})
	e.mux = http.NewServeMux()
	NewHandler(e.svc, stubSession, HandlerOptions{}).Register(e.mux)
	return e
}

func (e *testEnv) importer(t *testing.T, opts ImportOptions) *Importer {
	t.Helper()
	opts.Logger = e.logger
	if opts.DayDelay == 0 {
		opts.DayDelay = -1 // no pause in tests
	}
	opts.Now = e.clock
	return NewImporter(e.st, e.box, e.client, e.sink, opts)
}

// stubSession stands in for auth.Service.RequireSession: the test names the
// user in a header.
func stubSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.Header.Get("X-Test-User"))
		if err != nil {
			httpapi.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		next.ServeHTTP(w, r.WithContext(httpapi.WithUser(r.Context(), httpapi.CurrentUser{ID: id, Via: "session"})))
	})
}

func (e *testEnv) newUser(t *testing.T, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := e.st.Pool.QueryRow(context.Background(),
		"INSERT INTO users (hca_id, username, email) VALUES ($1, $2, $3) RETURNING id",
		"hca-"+name, name, name+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}
	return id
}

func (e *testEnv) do(t *testing.T, method, target string, user uuid.UUID, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	if user != uuid.Nil {
		req.Header.Set("X-Test-User", user.String())
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// startLink calls GET /auth/hackatime/link and returns the authorize URL and state cookie.
func (e *testEnv) startLink(t *testing.T, user uuid.UUID) (string, *http.Cookie) {
	t.Helper()
	rec := e.do(t, http.MethodGet, "/auth/hackatime/link", user)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/hackatime/link = %d %s", rec.Code, rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no state cookie set")
	}
	return rec.Header().Get("Location"), cookie
}

// link runs the whole browser flow for user and Hackatime account hid.
func (e *testEnv) link(t *testing.T, user uuid.UUID, hid int64) *httptest.ResponseRecorder {
	t.Helper()
	authURL, cookie := e.startLink(t, user)
	code, state := e.fake.authorize(authURL, hid)
	return e.do(t, http.MethodGet, "/auth/hackatime/callback?code="+code+"&state="+state, user, cookie)
}

// linkDirect runs the link flow through the Service without the HTTP layer or
// *testing.T, so it can run inside a fake Hackatime hook.
func (e *testEnv) linkDirect(ctx context.Context, user uuid.UUID, hid int64) error {
	authURL, cookie, err := e.svc.BeginLink(user)
	if err != nil {
		return err
	}
	code, state := e.fake.authorize(authURL, hid)
	return e.svc.CompleteLink(ctx, user, cookie, state, code)
}

// tokenFromDB decrypts the stored token, failing if it isn't bound to user.
func (e *testEnv) tokenFromDB(t *testing.T, user uuid.UUID) string {
	t.Helper()
	link, err := e.st.GetHackatimeLink(context.Background(), user)
	if err != nil {
		t.Fatalf("reading link: %v", err)
	}
	tok, err := e.box.Open(link.AccessTokenEnc, user[:])
	if err != nil {
		t.Fatalf("decrypting token: %v", err)
	}
	return string(tok)
}

func (e *testEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func assertNoSecret(t *testing.T, where, haystack, secret string) {
	t.Helper()
	if secret != "" && strings.Contains(haystack, secret) {
		t.Fatalf("%s contains the access token", where)
	}
}
