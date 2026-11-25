package submissions_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/jobs"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/internal/server/submissions"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/submission"
)

const webURL = "https://web.test"

// appHeader stands in for a client-credentials token: the app it names
// is the caller (stubScope).
const appHeader = "X-Test-App"

// stubScope stands in for oauth.Service.RequireScope("submissions"): it
// puts a client-credentials token of the app in appHeader on the context.
func stubScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app, err := uuid.Parse(r.Header.Get(appHeader))
		if err != nil {
			httpapi.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing access token"})
			return
		}
		ctx := httpapi.WithOAuthToken(r.Context(), httpapi.OAuthToken{ID: app, AppID: app, Scopes: []string{oauth.ScopeSubmissions}})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// fakeTrust is the Hackatime trust refresher: it records calls and either
// fails (Hackatime unreachable) or stores level as freshly read.
type fakeTrust struct {
	mu    sync.Mutex
	st    *store.Store
	level string // stored on a refresh; "" keeps the stored level
	fail  bool
	calls int
}

func (f *fakeTrust) Refresh(ctx context.Context, user uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return context.DeadlineExceeded
	}
	_, err := f.st.Pool.Exec(ctx, `UPDATE hackatime_links SET trust_level = coalesce(nullif($2, ''), trust_level),
		trust_checked_at = $3 WHERE user_id = $1`, user, f.level, now)
	return err
}

func (f *fakeTrust) set(level string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.level, f.fail, f.calls = level, fail, 0
}

type env struct {
	t       *testing.T
	st      *store.Store
	box     *secretbox.Box
	svc     *submissions.Service
	checker *submissions.Checker
	slots   *submissions.DecodeSlots
	// uploadWait is HandlerOptions.UploadWait (0 for the default).
	uploadWait time.Duration
	oauth      *oauth.Service
	trust      *fakeTrust
	mux        *http.ServeMux
	clock      time.Time
}

func newEnv(t *testing.T, opts ...func(*submissions.Options)) *env {
	t.Helper()
	st := storetest.New(t)
	box, err := secretbox.New(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, box: box, clock: now, trust: &fakeTrust{st: st}}
	clock := func() time.Time { return e.clock }
	e.oauth = oauth.NewService(st, oauth.Options{WebURL: webURL, Now: clock})
	e.slots = submissions.NewDecodeSlots(1)
	e.checker = submissions.NewChecker(st, ledger.NewService(st, ledger.WithClock(clock)),
		submissions.CheckerOptions{DecodeSlots: e.slots, Now: clock})
	workers := river.NewWorkers()
	if err := submissions.AddWorkers(workers, e.checker); err != nil {
		t.Fatal(err)
	}
	jc, err := jobs.New(st.Pool, jobs.Options{Workers: workers, Queues: map[string]int{submissions.Queue: 1}})
	if err != nil {
		t.Fatal(err)
	}
	o := submissions.Options{
		ReviewBaseURL: webURL, Enqueuer: jc.River(), Members: e.oauth, Trust: e.trust, DecodeSlots: e.slots, Now: clock,
	}
	for _, f := range opts {
		f(&o)
	}
	e.svc = submissions.NewService(st, box, o)
	e.resetLimits()
	return e
}

// resetLimits mounts the routes on a new mux, with fresh rate limits.
func (e *env) resetLimits() {
	e.mux = http.NewServeMux()
	submissions.NewHandler(e.svc, submissions.HandlerOptions{
		RequireSession: httpapitest.StubSession, RequireAPIKey: httpapitest.StubSession,
		RequireSubmissionsScope: stubScope, ClientKey: func(*http.Request) string { return "client" },
		UploadWait: e.uploadWait,
	}).Register(e.mux)
}

// call is one request: user signs it (uuid.Nil for none), app names a
// client-credentials token, gz compresses body.
type call struct {
	method, target string
	user, app      uuid.UUID
	body           []byte
	gz             bool
	// encoding is sent as Content-Encoding with body as it is.
	encoding string
}

func (e *env) do(c call) *httptest.ResponseRecorder {
	e.t.Helper()
	if c.method == "" {
		c.method = http.MethodGet
	}
	body := c.body
	if c.gz {
		body = gz(e.t, body)
	}
	r := httpapitest.Request(e.t, c.method, c.target, c.user, string(body))
	if c.gz {
		r.Header.Set("Content-Encoding", "gzip")
	}
	if c.encoding != "" {
		r.Header.Set("Content-Encoding", c.encoding)
	}
	if c.app != uuid.Nil {
		r.Header.Set(appHeader, c.app.String())
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, r)
	return rec
}

func (e *env) expect(c call, want int) []byte {
	e.t.Helper()
	rec := e.do(c)
	if rec.Code != want {
		e.t.Fatalf("%s %s = %d %s, want %d", c.method, c.target, rec.Code, rec.Body, want)
	}
	return rec.Body.Bytes()
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
	return v
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.st.Pool.Exec(context.Background(), query, args...); err != nil {
		e.t.Fatal(err)
	}
}

// link gives user a Hackatime link at level, checked at checked.
func (e *env) link(user uuid.UUID, hackatimeID int64, level string, checked time.Time) {
	e.t.Helper()
	e.exec(`INSERT INTO hackatime_links (user_id, hackatime_user_id, hackatime_username, access_token_enc, trust_level, trust_checked_at)
		VALUES ($1, $2, 'ht', '\x00', $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET trust_level = EXCLUDED.trust_level, trust_checked_at = EXCLUDED.trust_checked_at`,
		user, hackatimeID, level, checked)
}

// program registers a public app owned by owner with reviewers.
func (e *env) program(owner uuid.UUID, slug string, public bool, reviewers ...string) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	a, err := e.oauth.CreateApp(ctx, owner, oauth.AppInput{
		Name: "Program " + slug, Slug: slug, RedirectURIs: []string{"https://program.test/cb"}, Public: public, Confidential: true,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range reviewers {
		if _, err := e.oauth.AddMember(ctx, owner, a.ID, r); err != nil {
			e.t.Fatal(err)
		}
	}
	return a.ID
}

// world is the standard setup: alice works on app on device A (with
// evidence) and credits time on device B (without), and terra is a public
// program owned by fiona with reviewer rita.
type world struct {
	*env
	alice, bob, fiona, rita, mallory uuid.UUID
	a, b                             *evDevice
	terra                            uuid.UUID
	// bRows are B's frozen rows of app; bLib is B's lib row at 09:30.
	bRows []string
}

func newWorld(t *testing.T, opts ...func(*submissions.Options)) *world {
	t.Helper()
	e := newEnv(t, opts...)
	w := &world{env: e}
	w.alice = seedtest.User(t, e.st, "alice", "UTC", true)
	w.bob = seedtest.User(t, e.st, "bob", "UTC", true)
	w.fiona = seedtest.User(t, e.st, "fiona", "UTC", true)
	w.rita = seedtest.User(t, e.st, "rita", "UTC", true)
	w.mallory = seedtest.User(t, e.st, "mallory", "UTC", true)
	w.terra = e.program(w.fiona, "terra", true, "rita")
	e.link(w.alice, 4321, "green", now.Add(-time.Hour))

	w.a = e.register(w.alice, "laptop", 1)
	e.work(w.alice, w.a)
	w.b = e.register(w.alice, "desktop", 2)
	e.sealOther(w.alice, w.b)
	// B credits lib for half of 09:30 (the cap shares the minute with A's
	// app) and app at 09:40 to 09:44 (no evidence: unbacked).
	key := func(m time.Time) (*time.Time, *int64) {
		tt, h := m.Truncate(10*time.Minute).Add(40*time.Minute), int64(5)
		return &tt, &h
	}
	tt, h := key(at(9, 30))
	e.credit(w.alice, minuteRow(w.b.id, at(9, 30), "lib", 30, tt, h))
	for i := range 5 {
		m := at(9, 40+i)
		tt, h := key(m)
		e.credit(w.alice, minuteRow(w.b.id, m, "app", 60, tt, h))
		w.bRows = append(w.bRows, m.Format(time.RFC3339))
	}
	// Time at the keyboard for app: an hour, ten minutes of it with AI.
	seedtest.Span(t, e.st, w.alice, at(9, 0), 50, seedtest.Minute{Project: "app"})
	seedtest.Span(t, e.st, w.alice, at(9, 50), 10, seedtest.Minute{Project: "app", Category: "ai coding"})
	return w
}

func minuteRow(device uuid.UUID, m time.Time, project string, credited int, t *time.Time, h *int64) submission.Minute {
	return submission.Minute{DeviceID: device.String(), Minute: m, Project: project, Label: "you", Credited: credited,
		Cuts: []string{}, Flags: []string{}, RulesVersion: struggle.RulesVersion, ComputedAt: t, Snapshot: h}
}

// upload is alice's upload of app for the submitted day with A's part.
func (w *world) upload(program string) []byte {
	return mustJSON(w.t, submission.Upload{Project: "app", From: submittedDay, To: submittedDay, Program: program,
		Part: w.a.part(w.t, partOpts{project: "app"})})
}

type accepted struct {
	ID        uuid.UUID `json:"id"`
	Status    string    `json:"status"`
	ReviewURL string    `json:"review_url"`
}

// submit creates alice's submission and returns it with its token.
func (w *world) submit(program string) (accepted, string) {
	w.t.Helper()
	out := decode[accepted](w.t, w.expect(call{method: http.MethodPost, target: "/api/v1/submissions", user: w.alice,
		body: w.upload(program), gz: true}, http.StatusAccepted))
	token, ok := strings.CutPrefix(out.ReviewURL, webURL+"/r/")
	if !ok || out.Status != "verifying" {
		w.t.Fatalf("created = %+v", out)
	}
	return out, token
}

// verify runs the re-check job.
func (e *env) verify(id uuid.UUID) {
	e.t.Helper()
	if err := e.checker.Verify(context.Background(), id); err != nil {
		e.t.Fatal(err)
	}
}

// audit returns actor's audit entries about submissions.
func (e *env) audit(actor uuid.UUID) []seedtest.AuditEntry {
	e.t.Helper()
	var out []seedtest.AuditEntry
	for _, a := range seedtest.Audit(e.t, e.st, actor) {
		if a.SubjectType == "submission" {
			out = append(out, a)
		}
	}
	return out
}

func (e *env) status(id uuid.UUID) string {
	e.t.Helper()
	var s string
	if err := e.st.Pool.QueryRow(context.Background(), `SELECT status FROM submissions WHERE id = $1`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}
