package hackatime

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

var day1 = time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

// perDay makes every day with activity, and the day after it, a window of
// its own, for tests about what happens between days.
var perDay = ImportOptions{WindowActivity: time.Nanosecond}

// linkedUser links a new user to Hackatime account hid through the real flow.
func (e *testEnv) linkedUser(t *testing.T, name string, hid int64) uuid.UUID {
	t.Helper()
	e.fake.addUser(hid, "green")
	u := e.newUser(t, name)
	if rec := e.link(t, u, hid); rec.Code != http.StatusSeeOther {
		t.Fatalf("link = %d %s", rec.Code, rec.Body)
	}
	return u
}

func TestImportAcrossDays(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()

	e.fake.addHeartbeat(42, day1.Add(10*time.Hour), "/a.go", map[string]any{
		"ai_model": "claude-x", "ai_session": "s1", "ai_input_tokens": 1200, "ai_output_tokens": 300,
		"ai_prompt_length": 80, "ai_line_changes": 12, "human_line_changes": 3, "category": "ai coding",
	})
	e.fake.addHeartbeat(42, day1.Add(23*time.Hour+59*time.Minute+59*time.Second+500*time.Millisecond), "/edge.go", nil)
	e.fake.addHeartbeat(42, day1.Add(24*time.Hour), "/b.go", nil) // midnight belongs to day 2 only
	e.fake.addHeartbeat(42, day1.Add(36*time.Hour), "/test.go", map[string]any{"source_type": "test_entry"})
	e.fake.addHeartbeat(42, day1.Add(50*time.Hour), "/c.go", nil)
	e.fake.addHeartbeat(42, day1.Add(-time.Hour), "/before.go", nil) // before from_date

	res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false)
	if err != nil || !res.Done {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if got := e.sink.count(alice); got != 4 {
		t.Fatalf("imported %d heartbeats, want 4", got)
	}
	// Three quiet days fit in one window: one probe, one heartbeats request.
	if calls := e.fake.heartbeatCalls(); len(calls) != 1 || calls[0] != "2025-03-01T00:00:00Z..2025-03-03T23:59:59Z" {
		t.Fatalf("heartbeat calls = %v, want one covering all three days", calls)
	}
	if calls := e.fake.spanCalls(); len(calls) != 1 {
		t.Fatalf("span calls = %v, want one probe", calls)
	}
	var ai bool
	for _, h := range e.sink.byUser[alice] {
		if h.AIModel == "claude-x" {
			ai = h.AISession == "s1" && *h.AIInputTokens == 1200 && *h.AIOutputTokens == 300 &&
				*h.AIPromptLength == 80 && *h.AILineChanges == 12 && *h.HumanLineChanges == 3 && h.Category == "ai coding"
		}
	}
	if !ai {
		t.Fatal("AI fields not carried over")
	}
	for _, s := range e.sink.sources {
		if s != ImportSource {
			t.Fatalf("source = %q", s)
		}
	}
	imp, err := e.st.GetHackatimeImport(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Status != importDone || imp.HeartbeatsImported != 4 || imp.NextDate.Time.Format(time.DateOnly) != "2025-03-03" {
		t.Fatalf("import = %+v", imp)
	}
	// Done imports are not re-run.
	if res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("second Run() = %+v, %v", res, err)
	}
	if calls := e.fake.heartbeatCalls(); len(calls) != 1 {
		t.Fatalf("done import fetched again: %v", calls)
	}
}

func TestImportChunksIngest(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	for i := range 2500 {
		e.fake.addHeartbeat(42, day1.Add(time.Duration(i)*time.Second), "/f.go", nil)
	}
	if _, err := e.importer(t, ImportOptions{}).Run(context.Background(), alice, false); err != nil {
		t.Fatal(err)
	}
	want := []int{1000, 1000, 500}
	if len(e.sink.chunks) != 3 || e.sink.chunks[0] != want[0] || e.sink.chunks[1] != want[1] || e.sink.chunks[2] != want[2] {
		t.Fatalf("chunks = %v, want %v", e.sink.chunks, want)
	}
	if e.sink.count(alice) != 2500 {
		t.Fatalf("imported %d", e.sink.count(alice))
	}
}

func TestImportResumesAfterFailure(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	for d := range 3 {
		e.fake.addHeartbeat(42, day1.Add(time.Duration(d)*24*time.Hour+time.Hour), "/x.go", nil)
	}
	e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxRetries = -1 })
	failing := true
	e.fake.hbHook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if failing && r.URL.Query().Get("start_time") == "2025-03-02T00:00:00Z" {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}

	_, err := e.importer(t, perDay).Run(ctx, alice, false)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("Run() err = %v, want status error", err)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importPending || imp.NextDate.Time.Format(time.DateOnly) != "2025-03-02" ||
		imp.HeartbeatsImported != 1 || imp.LastError == "" {
		t.Fatalf("import after failure = %+v", imp)
	}

	failing = false
	res, err := e.importer(t, perDay).Run(ctx, alice, false)
	if err != nil || !res.Done {
		t.Fatalf("resumed Run() = %+v, %v", res, err)
	}
	if e.sink.count(alice) != 3 {
		t.Fatalf("imported %d, want 3", e.sink.count(alice))
	}
	calls := e.fake.heartbeatCalls()
	if len(calls) != 3 { // day 1 once, day 2 after recovery, day 3
		t.Fatalf("calls = %v; day 1 should not be refetched", calls)
	}
	imp, _ = e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || imp.LastError != "" || imp.HeartbeatsImported != 3 {
		t.Fatalf("import = %+v", imp)
	}
}

func TestImportSinkFailureThenIdempotentRerun(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	for d := range 3 {
		e.fake.addHeartbeat(42, day1.Add(time.Duration(d)*24*time.Hour+time.Hour), "/x.go", nil)
	}
	e.sink.failOn = 2 // day 2's ingest fails
	if _, err := e.importer(t, perDay).Run(ctx, alice, false); !errors.Is(err, errSinkDown) {
		t.Fatalf("Run() err = %v", err)
	}
	if _, err := e.importer(t, perDay).Run(ctx, alice, false); err != nil {
		t.Fatal(err)
	}
	// Relinking re-runs the window; dedup in the sink keeps it at 3.
	if _, err := e.st.Pool.Exec(ctx, "UPDATE hackatime_imports SET next_date = from_date, status = 'pending' WHERE user_id = $1", alice); err != nil {
		t.Fatal(err)
	}
	if _, err := e.importer(t, perDay).Run(ctx, alice, false); err != nil {
		t.Fatal(err)
	}
	if e.sink.count(alice) != 3 {
		t.Fatalf("imported %d after re-run, want 3 (no duplicates)", e.sink.count(alice))
	}
}

func TestImportFailureStates(t *testing.T) {
	tests := []struct {
		name        string
		hook        func(w http.ResponseWriter)
		lastAttempt bool
		wantStatus  string
		wantDone    bool
	}{
		{"transient error stays pending", func(w http.ResponseWriter) { w.WriteHeader(502) }, false, importPending, false},
		{"last attempt marks failed", func(w http.ResponseWriter) { w.WriteHeader(502) }, true, importFailed, true},
		{"revoked token fails at once", func(w http.ResponseWriter) { w.WriteHeader(401) }, false, importFailed, true},
		{"rate limit stays pending even on last attempt", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(429)
		}, true, importPending, false},
		{"malformed response retried", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"heartbeats": [`)) }, false, importPending, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.linkedUser(t, "alice", 42)
			e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxRetries = -1 })
			e.fake.hbHook = func(w http.ResponseWriter, _ *http.Request, _ int) bool { tt.hook(w); return true }
			res, err := e.importer(t, ImportOptions{}).Run(context.Background(), alice, tt.lastAttempt)
			if err == nil || res.Done != tt.wantDone {
				t.Fatalf("Run() = %+v, %v", res, err)
			}
			imp, _ := e.st.GetHackatimeImport(context.Background(), alice)
			if imp.Status != tt.wantStatus || imp.LastError == "" {
				t.Fatalf("import = %+v, want status %s with an error", imp, tt.wantStatus)
			}
			token := e.tokenFromDB(t, alice)
			assertNoSecret(t, "last_error", imp.LastError, token)
			assertNoSecret(t, "logs", e.logs.String(), token)
		})
	}
}

func TestImportSplitsLargeDays(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	for i := range 40 {
		e.fake.addHeartbeat(42, day1.Add(time.Duration(i)*30*time.Minute), "/f.go", nil)
	}
	e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxHeartbeatsBody = 16 << 10 })
	if _, err := e.importer(t, ImportOptions{}).Run(context.Background(), alice, false); err != nil {
		t.Fatal(err)
	}
	if e.sink.count(alice) != 40 {
		t.Fatalf("imported %d, want 40", e.sink.count(alice))
	}
	if len(e.fake.heartbeatCalls()) <= 3 {
		t.Fatal("expected the busy day to be split into smaller windows")
	}
}

func TestImportYieldsAfterBudget(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	for d := range 3 {
		e.fake.addHeartbeat(42, day1.Add(time.Duration(d)*24*time.Hour+time.Hour), "/x.go", nil)
	}
	e.fake.hbHook = func(_ http.ResponseWriter, _ *http.Request, _ int) bool {
		time.Sleep(5 * time.Millisecond)
		return false
	}
	res, err := e.importer(t, ImportOptions{RunBudget: time.Millisecond, WindowActivity: time.Nanosecond}).Run(ctx, alice, false)
	if err != nil || res.Done {
		t.Fatalf("Run() = %+v, %v; want not done", res, err)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importRunning || imp.NextDate.Time.Format(time.DateOnly) != "2025-03-02" {
		t.Fatalf("import = %+v; want one day done", imp)
	}
}

func TestImportStopsWhenUnlinked(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	if err := e.svc.Unlink(ctx, alice); err != nil {
		t.Fatal(err)
	}
	res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false)
	if err != nil || !res.Done || len(e.fake.heartbeatCalls()) != 0 {
		t.Fatalf("Run() after unlink = %+v, %v, calls %v", res, err, e.fake.heartbeatCalls())
	}
}
