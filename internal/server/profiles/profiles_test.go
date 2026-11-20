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

	"github.com/apneduniya/logbook/internal/server/profiles"
	"github.com/apneduniya/logbook/internal/server/stats/statstest"
	"github.com/apneduniya/logbook/internal/server/store"
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
	pub      uuid.UUID // public profile with data
	priv     uuid.UUID // private profile
}

func setup(t *testing.T) fixture {
	t.Helper()
	st := storetest.New(t)
	pub := statstest.User(t, st, "alice", "UTC", true)
	priv := statstest.User(t, st, "bob", "UTC", false)

	now := time.Now().UTC().Truncate(time.Minute)
	old := now.Add(-30 * 24 * time.Hour)
	statstest.Span(t, st, pub, old, 60, statstest.Minute{Project: "logbook", Language: "Go"})
	statstest.Span(t, st, pub, now.Add(-time.Hour), 30, statstest.Minute{Project: "logbook", Language: "SQL"})
	statstest.Span(t, st, pub, now.Add(-2*time.Hour), 20, statstest.Minute{Project: "secret-project", Language: "Rust"})
	statstest.Span(t, st, pub, now.Add(-3*time.Hour), 10, statstest.Minute{Project: "logbook", Label: "agent"})
	statstest.Span(t, st, priv, now.Add(-time.Hour), 10, statstest.Minute{})
	if _, err := st.Pool.Exec(context.Background(), `
		INSERT INTO projects (user_id, name, repo_url, hidden) VALUES
		($1, 'logbook', 'https://github.com/apneduniya/logbook', false),
		($1, 'secret-project', 'https://github.com/alice/secret', true)`, pub); err != nil {
		t.Fatal(err)
	}

	l := &recordingListener{}
	mux := http.NewServeMux()
	profiles.NewHandler(profiles.NewService(st, l), statstest.StubSession).Register(mux)
	return fixture{st: st, mux: mux, listener: l, pub: pub, priv: priv}
}

func (f fixture) do(t *testing.T, method, target string, user uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, statstest.Request(t, method, target, user, body))
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
	ctx := context.Background()
	var alicePublic bool
	if err := f.st.Pool.QueryRow(ctx, `SELECT profile_public FROM users WHERE id = $1`, f.pub).Scan(&alicePublic); err != nil {
		t.Fatal(err)
	}
	if alicePublic {
		t.Fatal("bob's patch changed alice")
	}

	// display_name x2 (+ clear), timezone, display_name+public for alice; public for bob.
	var n int
	if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'profile.update' AND subject_id = $1`, f.pub.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("alice audit rows = %d, want 4", n)
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
