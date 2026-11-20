package profiles_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/profiles"
	"github.com/apneduniya/logbook/internal/server/projects"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

type recordingListener struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (l *recordingListener) ProfileMadePrivate(id uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids = append(l.ids, id)
}

type fixture struct {
	st       *store.Store
	mux      *http.ServeMux
	listener *recordingListener
	svc      *profiles.Service
	projects *projects.Service
	pub      uuid.UUID // public profile with data
	priv     uuid.UUID // private profile
}

func setup(t *testing.T) fixture {
	t.Helper()
	st := storetest.New(t)
	pub := seedtest.User(t, st, "alice", "UTC", true)
	priv := seedtest.User(t, st, "bob", "UTC", false)

	now := time.Now().UTC().Truncate(time.Minute)
	old := now.Add(-30 * 24 * time.Hour)
	seedtest.Span(t, st, pub, old, 60, seedtest.Minute{Project: "logbook", Language: "Go"})
	seedtest.Span(t, st, pub, now.Add(-time.Hour), 30, seedtest.Minute{Project: "logbook", Language: "SQL"})
	seedtest.Span(t, st, pub, now.Add(-2*time.Hour), 20, seedtest.Minute{Project: "secret-project", Language: "Rust"})
	seedtest.Span(t, st, pub, now.Add(-3*time.Hour), 10, seedtest.Minute{Project: "logbook", Label: "agent"})
	seedtest.Span(t, st, priv, now.Add(-time.Hour), 10, seedtest.Minute{})
	l := &recordingListener{}
	svc := profiles.NewService(st, l)
	projectSvc := projects.NewService(st, svc)
	setProject(t, projectSvc, pub, "logbook", "https://github.com/apneduniya/logbook", false)
	setProject(t, projectSvc, pub, "secret-project", "https://github.com/alice/secret", true)

	mux := http.NewServeMux()
	profiles.NewHandler(svc, httpapitest.StubSession, httpapitest.NoLimit).Register(mux)
	return fixture{st: st, mux: mux, listener: l, svc: svc, projects: projectSvc, pub: pub, priv: priv}
}

func setProject(t *testing.T, svc *projects.Service, user uuid.UUID, name, repo string, hidden bool) {
	t.Helper()
	if _, err := svc.Update(context.Background(), user, name, projects.Patch{RepoURL: &repo, Hidden: &hidden}); err != nil {
		t.Fatalf("setting project %s: %v", name, err)
	}
}

func (f fixture) do(t *testing.T, method, target string, user uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httpapitest.Request(t, method, target, user, body))
	return rec
}

func TestPublicProfile(t *testing.T) {
	f := setup(t)
	rec := f.do(t, http.MethodGet, "/api/v1/users/ALICE/profile", uuid.Nil, "")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	for _, leak := range []string{"secret", "Rust", "@example.com", "hca-", "email", "hca_id", "timezone"} {
		if strings.Contains(raw, leak) {
			t.Errorf("public profile contains %q: %s", leak, raw)
		}
	}
	var p profiles.Public
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.Username != "alice" || p.DisplayName != "Name alice" || p.MemberSince.IsZero() {
		t.Errorf("identity = %+v", p)
	}
	if p.Totals != (profiles.Totals{AllTimeSeconds: 90 * 60, Last7DaysSeconds: 30 * 60}) {
		t.Errorf("totals = %+v", p.Totals)
	}
	if len(p.Projects) != 1 || p.Projects[0].Name != "logbook" || p.Projects[0].RepoURL != "https://github.com/apneduniya/logbook" {
		t.Errorf("projects = %+v", p.Projects)
	}
	if len(p.Languages) != 2 || p.Languages[0] != (profiles.Item{Name: "Go", Seconds: 3600}) {
		t.Errorf("languages = %+v", p.Languages)
	}
}

func TestPublicProfileNotFoundIsUniform(t *testing.T) {
	f := setup(t)
	var bodies []string
	for _, name := range []string{"bob", "nobody", "x", "bad%20name", strings.Repeat("a", 40)} {
		rec := f.do(t, http.MethodGet, "/api/v1/users/"+name+"/profile", uuid.Nil, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", name, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("404 bodies differ: %q vs %q", bodies[0], b)
		}
	}
	// The owner can't see their private profile through the public route either.
	if rec := f.do(t, http.MethodGet, "/api/v1/users/bob/profile", f.priv, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("owner status = %d, want 404", rec.Code)
	}
}

func TestMeProfileIsolation(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		user uuid.UUID
		want string
	}{{f.pub, "alice"}, {f.priv, "bob"}} {
		rec := f.do(t, http.MethodGet, "/api/v1/me/profile", tc.user, "")
		if rec.Code != 200 {
			t.Fatalf("status = %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "@example.com") || strings.Contains(rec.Body.String(), "hca-") {
			t.Fatalf("me profile leaks email or hca id: %s", rec.Body)
		}
		var m profiles.Me
		if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m.Username != tc.want {
			t.Fatalf("user %v got profile %q", tc.user, m.Username)
		}
	}
	if rec := f.do(t, http.MethodGet, "/api/v1/me/profile", uuid.Nil, ""); rec.Code != 401 {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}
}

func TestPatchProfile(t *testing.T) {
	f := setup(t)
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{"display name", `{"display_name":"  Alice A.  "}`, 200},
		{"timezone", `{"timezone":"Asia/Kolkata"}`, 200},
		{"empty display name allowed", `{"display_name":""}`, 200},
		{"too long name", fmt.Sprintf(`{"display_name":%q}`, strings.Repeat("é", 65)), 400},
		{"control char", `{"display_name":"a\u0007b"}`, 400},
		{"bidi override", `{"display_name":"a\u202eb"}`, 400},
		{"bad timezone", `{"timezone":"Mars/Olympus"}`, 400},
		{"local timezone", `{"timezone":"Local"}`, 400},
		{"empty timezone", `{"timezone":""}`, 400},
		{"empty patch", `{}`, 400},
		{"bad json", `{"profile_public":"no"}`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPatch, "/api/v1/me/profile", f.pub, tt.body)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
		})
	}
	rec := f.do(t, http.MethodPatch, "/api/v1/me/profile", f.pub, `{"display_name":"Alice","profile_public":false}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var m profiles.Me
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.DisplayName != "Alice" || m.Timezone != "Asia/Kolkata" || m.ProfilePublic {
		t.Fatalf("after patch = %+v", m)
	}
	if len(f.listener.ids) != 1 || f.listener.ids[0] != f.pub {
		t.Fatalf("listener calls = %v", f.listener.ids)
	}
	if rec := f.do(t, http.MethodGet, "/api/v1/users/alice/profile", uuid.Nil, ""); rec.Code != 404 {
		t.Fatalf("profile still public: %d", rec.Code)
	}

	// Bob's patch only changes Bob.
	if rec := f.do(t, http.MethodPatch, "/api/v1/me/profile", f.priv, `{"profile_public":true}`); rec.Code != 200 {
		t.Fatalf("bob patch status = %d", rec.Code)
	}
	alice, err := f.st.ProfileGetByID(context.Background(), f.pub)
	if err != nil {
		t.Fatal(err)
	}
	if alice.ProfilePublic {
		t.Fatal("bob's patch changed alice")
	}

	// display_name, timezone, clear display_name, display_name+public for
	// alice (her two project settings are audited too); public for bob.
	if n := countAudit(seedtest.Audit(t, f.st, f.pub), "profile.update", f.pub.String()); n != 4 {
		t.Fatalf("alice audit rows = %d, want 4", n)
	}
	if n := countAudit(seedtest.Audit(t, f.st, f.priv), "profile.update", f.priv.String()); n != 1 {
		t.Fatalf("bob audit rows = %d, want 1", n)
	}
}

func countAudit(rows []seedtest.AuditEntry, action, subject string) int {
	n := 0
	for _, r := range rows {
		if r.Action == action && r.SubjectID == subject {
			n++
		}
	}
	return n
}

// Concurrent PATCHes are serialised by the row lock, so every audit entry's
// "from" is the previous entry's "to".
func TestConcurrentPatchesKeepAuditChain(t *testing.T) {
	f := setup(t)
	const n = 12
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			body := fmt.Sprintf(`{"display_name":"name %d"}`, i)
			if rec := f.do(t, http.MethodPatch, "/api/v1/me/profile", f.pub, body); rec.Code != 200 {
				t.Errorf("status = %d: %s", rec.Code, rec.Body)
			}
		})
	}
	wg.Wait()
	prev := "Name alice"
	seen := 0
	for _, r := range seedtest.Audit(t, f.st, f.pub) {
		if r.Action != "profile.update" {
			continue
		}
		var d struct {
			Changes map[string]struct{ From, To string } `json:"changes"`
		}
		if err := json.Unmarshal([]byte(r.Data), &d); err != nil {
			t.Fatal(err)
		}
		c := d.Changes["display_name"]
		if c.From != prev {
			t.Fatalf("entry %d: from = %q, want %q (previous to)", seen, c.From, prev)
		}
		prev = c.To
		seen++
	}
	if seen != n {
		t.Fatalf("%d audit entries, want %d", seen, n)
	}
}

func TestPublicProfileCacheAndInvalidation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	get := func() profiles.Public {
		t.Helper()
		p, err := f.svc.Public(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := get()

	// New minutes don't show until the cache is refreshed...
	seedtest.Span(t, f.st, f.pub, time.Now().UTC().Add(-10*time.Minute), 5, seedtest.Minute{Project: "logbook"})
	if got := get(); got.Totals != first.Totals {
		t.Fatalf("totals changed without a refresh: %+v", got.Totals)
	}
	// ...which a live check asking for fresh data triggers.
	v1, ok, err := f.svc.PublicVersion(ctx, f.pub, time.Now())
	if err != nil || !ok {
		t.Fatalf("PublicVersion = %v, %v", ok, err)
	}
	if got := get(); got.Totals.AllTimeSeconds != first.Totals.AllTimeSeconds+300 {
		t.Fatalf("totals after refresh = %+v", got.Totals)
	}

	// Hiding a project drops it from the public profile at once.
	setProject(t, f.projects, f.pub, "logbook", "https://github.com/apneduniya/logbook", true)
	if got := get(); len(got.Projects) != 0 || got.Totals.AllTimeSeconds != 0 {
		t.Fatalf("hidden project still public: %+v", got)
	}
	v2, _, err := f.svc.PublicVersion(ctx, f.pub, time.Time{})
	if err != nil || v2 == v1 {
		t.Fatalf("version did not change after hiding: %q -> %q (%v)", v1, v2, err)
	}
}

func TestPublicVersion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	version := func(id uuid.UUID) (string, bool) {
		t.Helper()
		v, ok, err := f.svc.PublicVersion(ctx, id, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return v, ok
	}
	v0, ok := version(f.pub)
	if !ok || v0 == "" {
		t.Fatalf("public profile version = %q, %v", v0, ok)
	}
	at := time.Now().UTC().Add(-20 * time.Minute)
	seedtest.Span(t, f.st, f.pub, at, 3, seedtest.Minute{Project: "secret-project"})
	seedtest.Span(t, f.st, f.pub, at, 3, seedtest.Minute{Project: "logbook", Label: "agent", Language: "Zig"})
	seedtest.Span(t, f.st, f.pub, at, 3, seedtest.Minute{Project: "logbook", Label: "idle", Language: "Zig"})
	if v, _ := version(f.pub); v != v0 {
		t.Fatal("hidden, agent or idle minutes changed the public version")
	}
	seedtest.Span(t, f.st, f.pub, at, 3, seedtest.Minute{Project: "logbook"})
	if v, _ := version(f.pub); v == v0 {
		t.Fatal("visible minutes did not change the public version")
	}
	for name, id := range map[string]uuid.UUID{"private": f.priv, "missing": uuid.New()} {
		if v, ok := version(id); ok || v != "" {
			t.Errorf("%s: PublicVersion = %q, %v; want not ok", name, v, ok)
		}
	}
}

func TestPublicProfileUsesRateLimiter(t *testing.T) {
	f := setup(t)
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
	mux := http.NewServeMux()
	profiles.NewHandler(f.svc, httpapitest.StubSession, deny).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpapitest.Request(t, http.MethodGet, "/api/v1/users/alice/profile", uuid.Nil, ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httpapitest.Request(t, http.MethodGet, "/api/v1/me/profile", f.pub, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("/me status = %d; the public limiter must not apply", rec.Code)
	}
}

func TestPublicUserID(t *testing.T) {
	f := setup(t)
	svc := profiles.NewService(f.st, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ok   bool
	}{{"alice", true}, {"Alice", true}, {"bob", false}, {"nobody", false}, {"../x", false}} {
		_, ok, err := svc.PublicUserID(ctx, tc.name)
		if err != nil || ok != tc.ok {
			t.Errorf("PublicUserID(%q) = %v, %v; want ok=%v", tc.name, ok, err, tc.ok)
		}
	}
}
