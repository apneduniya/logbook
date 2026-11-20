package projects_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/projects"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

func TestValidateRepoURL(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{"", true},
		{"https://github.com/hackclub/hackatime", true},
		{"https://github.com/a/b.c_d-e", true},
		{"https://github.com/hack-club/x", true},
		{"http://github.com/hackclub/hackatime", false},
		{"https://github.com/hackclub", false},
		{"https://github.com/hackclub/hackatime/", false},
		{"https://github.com/hackclub/hackatime/issues", false},
		{"https://github.com/hackclub/hackatime?x=1", false},
		{"https://github.com/hackclub/hackatime#readme", false},
		{"https://github.com:443/hackclub/hackatime", false},
		{"https://user@github.com/hackclub/hackatime", false},
		{"https://gitlab.com/hackclub/hackatime", false},
		{"https://github.com.evil.com/hackclub/hackatime", false},
		{"https://github.com/-bad/repo", false},
		{"https://github.com/hackclub/..", false},
		{"https://github.com/hackclub/.", false},
		{"javascript:alert(1)", false},
		{"https://github.com/hackclub/hack atime", false},
	}
	for _, tt := range tests {
		if err := projects.ValidateRepoURL(tt.in); (err == nil) != tt.ok {
			t.Errorf("ValidateRepoURL(%q) err = %v, want ok=%v", tt.in, err, tt.ok)
		}
	}
}

type fixture struct {
	st       *store.Store
	mux      *http.ServeMux
	listener *recordingListener
	a, b     uuid.UUID
}

func setup(t *testing.T) fixture {
	t.Helper()
	st := storetest.New(t)
	a := seedtest.User(t, st, "alice", "UTC", true)
	b := seedtest.User(t, st, "bob", "UTC", true)
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	seedtest.Span(t, st, a, base, 30, seedtest.Minute{Project: "logbook", Language: "Go"})
	seedtest.Span(t, st, a, base.Add(time.Hour), 10, seedtest.Minute{Project: "logbook", Language: "SQL"})
	seedtest.Span(t, st, a, base.Add(-48*time.Hour), 5, seedtest.Minute{Project: "a/b", Language: "Python"})
	seedtest.Span(t, st, a, base.Add(2*time.Hour), 3, seedtest.Minute{Project: "agentonly", Label: "agent"})
	seedtest.Span(t, st, b, base, 60, seedtest.Minute{Project: "bobs"})
	l := &recordingListener{}
	mux := http.NewServeMux()
	projects.NewHandler(projects.NewService(st, l), httpapitest.StubSession).Register(mux)
	return fixture{st: st, mux: mux, listener: l, a: a, b: b}
}

func (f fixture) do(t *testing.T, method, target string, user uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httpapitest.Request(t, method, target, user, body))
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body, err)
	}
	return v
}

func TestListProjects(t *testing.T) {
	f := setup(t)
	rec := f.do(t, http.MethodGet, "/api/v1/me/projects", f.a, "")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decode[struct{ Projects []projects.Project }](t, rec).Projects
	if len(got) != 3 {
		t.Fatalf("got %d projects: %+v", len(got), got)
	}
	// Most recently active first.
	if got[0].Name != "agentonly" || got[1].Name != "logbook" || got[2].Name != "a/b" {
		t.Fatalf("order = %s, %s, %s", got[0].Name, got[1].Name, got[2].Name)
	}
	lb := got[1]
	if lb.TotalSeconds != 2400 || len(lb.Languages) != 2 || lb.Languages[0].Name != "Go" || lb.Languages[0].Seconds != 1800 {
		t.Fatalf("logbook = %+v", lb)
	}
	wantLast := time.Date(2026, 11, 10, 10, 10, 0, 0, time.UTC)
	if lb.LastActivity == nil || !lb.LastActivity.Equal(wantLast) {
		t.Fatalf("logbook last activity = %v, want %v", lb.LastActivity, wantLast)
	}
	if got[0].TotalSeconds != 0 || len(got[0].Languages) != 0 {
		t.Fatalf("agent-only project credited: %+v", got[0])
	}

	// Bob sees only his own project.
	rec = f.do(t, http.MethodGet, "/api/v1/me/projects", f.b, "")
	bobs := decode[struct{ Projects []projects.Project }](t, rec).Projects
	if len(bobs) != 1 || bobs[0].Name != "bobs" {
		t.Fatalf("bob's projects = %+v", bobs)
	}
	if rec := f.do(t, http.MethodGet, "/api/v1/me/projects", uuid.Nil, ""); rec.Code != 401 {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}
}

func TestGetProject(t *testing.T) {
	f := setup(t)
	tests := []struct {
		name   string
		user   uuid.UUID
		path   string
		status int
	}{
		{"own project", f.a, "/api/v1/me/projects/logbook", 200},
		{"escaped slash in name", f.a, "/api/v1/me/projects/a%2Fb", 200},
		{"missing", f.a, "/api/v1/me/projects/nope", 404},
		{"other user's project", f.b, "/api/v1/me/projects/logbook", 404},
		{"too long", f.a, "/api/v1/me/projects/" + longName(), 400},
		{"unauthenticated", uuid.Nil, "/api/v1/me/projects/logbook", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, tt.path, tt.user, "")
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
		})
	}
}

func longName() string {
	b := make([]byte, projects.MaxNameLen+1)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func TestPatchProject(t *testing.T) {
	f := setup(t)
	tests := []struct {
		name   string
		user   uuid.UUID
		path   string
		body   string
		status int
	}{
		{"set repo", f.a, "/api/v1/me/projects/logbook", `{"repo_url":"https://github.com/apneduniya/logbook"}`, 200},
		{"hide and archive", f.a, "/api/v1/me/projects/logbook", `{"hidden":true,"archived":true}`, 200},
		{"bad repo", f.a, "/api/v1/me/projects/logbook", `{"repo_url":"https://evil.com/x/y"}`, 400},
		{"empty patch", f.a, "/api/v1/me/projects/logbook", `{}`, 400},
		{"wrong type", f.a, "/api/v1/me/projects/logbook", `{"hidden":"yes"}`, 400},
		{"trailing data", f.a, "/api/v1/me/projects/logbook", `{"hidden":true} {}`, 400},
		{"unknown project", f.a, "/api/v1/me/projects/nope", `{"hidden":true}`, 404},
		{"other user's project", f.b, "/api/v1/me/projects/logbook", `{"hidden":false,"repo_url":""}`, 404},
		{"unauthenticated", uuid.Nil, "/api/v1/me/projects/logbook", `{"hidden":false}`, 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPatch, tt.path, tt.user, tt.body)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
		})
	}

	// Alice's settings survived Bob's attempt, and partial patches merged.
	rec := f.do(t, http.MethodGet, "/api/v1/me/projects/logbook", f.a, "")
	p := decode[projects.Project](t, rec)
	if p.RepoURL != "https://github.com/apneduniya/logbook" || !p.Hidden || !p.Archived || p.TotalSeconds != 2400 {
		t.Fatalf("after patches = %+v", p)
	}

	// Two effective changes are audited; the rejected ones are not.
	rows := seedtest.Audit(t, f.st, f.a)
	if len(rows) != 2 || rows[0].Action != "project.update" || rows[1].Action != "project.update" {
		t.Fatalf("audit rows = %+v, want 2 project.update", rows)
	}
	if rows := seedtest.Audit(t, f.st, f.b); len(rows) != 0 {
		t.Fatalf("bob has %d audit rows", len(rows))
	}
	if want := `{"name": "logbook", "changes": {"repo_url": {"to": "https://github.com/apneduniya/logbook", "from": ""}}}`; rows[0].Data != want {
		t.Fatalf("audit data = %s, want %s", rows[0].Data, want)
	}
	if got := f.listener.calls(); len(got) != 2 || got[0] != f.a || got[1] != f.a {
		t.Fatalf("listener calls = %v, want alice twice", got)
	}

	// A no-op patch writes no audit row and invalidates nothing.
	if rec := f.do(t, http.MethodPatch, "/api/v1/me/projects/logbook", f.a, `{"hidden":true}`); rec.Code != 200 {
		t.Fatalf("no-op status = %d", rec.Code)
	}
	if rows := seedtest.Audit(t, f.st, f.a); len(rows) != 2 {
		t.Fatalf("audit rows after no-op = %d, want 2", len(rows))
	}
	if got := f.listener.calls(); len(got) != 2 {
		t.Fatalf("listener called on no-op: %v", got)
	}
}

// Concurrent PATCHes are serialised by the row lock, so every audit entry's
// "from" is the previous entry's "to". The project has no settings row
// yet, so the first writers also race to create it.
func TestConcurrentPatchesKeepAuditChain(t *testing.T) {
	f := setup(t)
	const n = 12
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			body := fmt.Sprintf(`{"repo_url":"https://github.com/alice/repo%d"}`, i)
			if rec := f.do(t, http.MethodPatch, "/api/v1/me/projects/logbook", f.a, body); rec.Code != 200 {
				t.Errorf("status = %d: %s", rec.Code, rec.Body)
			}
		})
	}
	wg.Wait()
	prev := ""
	rows := seedtest.Audit(t, f.st, f.a)
	for i, r := range rows {
		var d struct {
			Changes map[string]struct{ From, To string } `json:"changes"`
		}
		if err := json.Unmarshal([]byte(r.Data), &d); err != nil {
			t.Fatal(err)
		}
		c := d.Changes["repo_url"]
		if c.From != prev {
			t.Fatalf("entry %d: from = %q, want %q (previous to)", i, c.From, prev)
		}
		prev = c.To
	}
	if len(rows) != n {
		t.Fatalf("%d audit entries, want %d", len(rows), n)
	}
}

type recordingListener struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (l *recordingListener) ProjectSettingsChanged(id uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids = append(l.ids, id)
}

func (l *recordingListener) calls() []uuid.UUID {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]uuid.UUID(nil), l.ids...)
}
