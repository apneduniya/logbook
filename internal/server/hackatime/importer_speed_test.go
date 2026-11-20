package hackatime

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// setImportStart moves a fresh import's start (and cursor) back to from, so a
// test can cover a long range without linking years ago.
func (e *testEnv) setImportStart(t *testing.T, userID uuid.UUID, from time.Time) {
	t.Helper()
	if _, err := e.st.Pool.Exec(context.Background(),
		"UPDATE hackatime_imports SET from_date = $2, next_date = $2 WHERE user_id = $1", userID, from); err != nil {
		t.Fatal(err)
	}
}

// addSession adds heartbeats every 30 seconds for length from start, and
// returns how many.
func (e *testEnv) addSession(hid int64, start time.Time, length time.Duration) int {
	n := 0
	for at := start; at.Before(start.Add(length)); at = at.Add(30 * time.Second) {
		e.fake.addHeartbeat(hid, at, "/s.go", nil)
		n++
	}
	return n
}

// TestImportRequestCountThreeYears imports three years in which the user was
// active on only 30 days. The old importer made one request per day (1,097
// here); probing and windowing needs a small fraction of that.
func TestImportRequestCountThreeYears(t *testing.T) {
	e := newEnv(t) // clock: 2025-03-03 15:00 UTC
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	from := time.Date(2022, 3, 3, 0, 0, 0, 0, time.UTC)
	e.setImportStart(t, alice, from)
	want := 0
	for i := range 30 {
		want += e.addSession(42, from.AddDate(0, 0, 18+i*36).Add(9*time.Hour), 4*time.Hour)
	}

	res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false)
	if err != nil || !res.Done {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if got := e.sink.count(alice); got != want {
		t.Fatalf("imported %d heartbeats, want %d", got, want)
	}
	reqs := e.fake.importRequests()
	t.Logf("requests for a 3-year range with 30 active days: %d (%d probes)", reqs, len(e.fake.spanCalls()))
	if reqs > 80 {
		t.Fatalf("made %d requests to Hackatime, want at most 80", reqs)
	}
}

func TestPlanWindows(t *testing.T) {
	d := func(i int) time.Time { return day1.AddDate(0, 0, i) }
	tests := []struct {
		name     string
		days     int
		activity map[int]time.Duration
		budget   time.Duration
		want     [][2]int
	}{
		{"all quiet is one window", 90, nil, 8 * time.Hour, [][2]int{{0, 90}}},
		{"quiet days join active ones under budget", 10, map[int]time.Duration{3: 2 * time.Hour, 7: 3 * time.Hour}, 8 * time.Hour, [][2]int{{0, 10}}},
		{"busy days split at the budget", 5, map[int]time.Duration{0: 5 * time.Hour, 1: 5 * time.Hour, 2: time.Hour, 4: 9 * time.Hour}, 8 * time.Hour, [][2]int{{0, 1}, {1, 4}, {4, 5}}},
		{"a day over budget closes its window", 3, map[int]time.Duration{1: 20 * time.Hour}, 8 * time.Hour, [][2]int{{0, 2}, {2, 3}}},
		{"tiny budget gives each active day its own window", 4, map[int]time.Duration{0: time.Minute, 1: time.Minute}, time.Nanosecond, [][2]int{{0, 1}, {1, 2}, {2, 4}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activity := map[time.Time]time.Duration{}
			for i, a := range tt.activity {
				activity[d(i)] = a
			}
			got := planWindows(d(0), d(tt.days), activity, tt.budget)
			if len(got) != len(tt.want) {
				t.Fatalf("planWindows() = %v, want %v", got, tt.want)
			}
			for i, w := range tt.want {
				if !got[i].from.Equal(d(w[0])) || !got[i].to.Equal(d(w[1])) {
					t.Fatalf("window %d = %v..%v, want days %d..%d", i, got[i].from, got[i].to, w[0], w[1])
				}
			}
		})
	}
}

// TestImportSkipsEmptyMonths covers a year that is empty until January. Each
// month costs one probe, the quiet months share one heartbeats request, and
// a lone heartbeat that no span reports is still imported.
func TestImportSkipsEmptyMonths(t *testing.T) {
	e := newEnv(t) // clock: 2025-03-03 15:00 UTC
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	from := time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC)
	e.setImportStart(t, alice, from)
	e.fake.addHeartbeat(42, time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC), "/lone.go", nil)
	want := 1
	want += e.addSession(42, time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC), 2*time.Hour)
	want += e.addSession(42, time.Date(2025, 1, 11, 23, 0, 0, 0, time.UTC), 2*time.Hour) // crosses midnight

	res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false)
	if err != nil || !res.Done {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if got := e.sink.count(alice); got != want {
		t.Fatalf("imported %d heartbeats, want %d", got, want)
	}
	days := int(time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC).Sub(from).Hours() / 24)
	if probes := len(e.fake.spanCalls()); probes != (days+probeDays-1)/probeDays {
		t.Fatalf("made %d probes for %d days, want one per %d days", probes, days, probeDays)
	}
	// Six quiet months in one window, then the rest (4h of activity) in another.
	if calls := e.fake.heartbeatCalls(); len(calls) != 2 {
		t.Fatalf("heartbeat calls = %v, want 2", calls)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || imp.HeartbeatsImported != int64(want) || imp.NextDate.Time.Format(time.DateOnly) != "2025-03-03" {
		t.Fatalf("import = %+v", imp)
	}
}

// TestImportProbeFailureFallsBack checks that a failed probe never skips
// data: the month it covered is fetched one day per request instead.
func TestImportProbeFailureFallsBack(t *testing.T) {
	tests := []struct {
		name string
		hook func(w http.ResponseWriter)
	}{
		{"server error", func(w http.ResponseWriter) { w.WriteHeader(502) }},
		{"user not found", func(w http.ResponseWriter) { writeJSON(w, 404, map[string]string{"error": "User not found"}) }},
		{"stats private", func(w http.ResponseWriter) {
			writeJSON(w, 403, map[string]string{"error": "user has disabled public stats"})
		}},
		{"malformed", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"spans": [{"start_time": "x"}]}`)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.linkedUser(t, "alice", 42)
			e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxRetries = -1 })
			e.fake.setSpansHook(func(w http.ResponseWriter, _ *http.Request) bool { tt.hook(w); return true })
			want := 0
			for d := range 3 {
				want += e.addSession(42, day1.Add(time.Duration(d)*24*time.Hour+time.Hour), time.Hour)
			}
			res, err := e.importer(t, ImportOptions{}).Run(context.Background(), alice, false)
			if err != nil || !res.Done {
				t.Fatalf("Run() = %+v, %v", res, err)
			}
			if got := e.sink.count(alice); got != want {
				t.Fatalf("imported %d, want %d", got, want)
			}
			if calls := e.fake.heartbeatCalls(); len(calls) != 3 {
				t.Fatalf("heartbeat calls = %v, want one per day", calls)
			}
			imp, _ := e.st.GetHackatimeImport(context.Background(), alice)
			if imp.Status != importDone || imp.LastError != "" {
				t.Fatalf("import = %+v", imp)
			}
		})
	}
}

// TestImportProbeRateLimited keeps 429 handling as it was: the import stays
// pending, even on the last attempt, and nothing is fetched or skipped.
func TestImportProbeRateLimited(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxRetries = -1 })
	e.fake.setSpansHook(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, true)
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 10*time.Minute || res.Done {
		t.Fatalf("Run() = %+v, %v; want rate limited for 10m", res, err)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importPending || !imp.NextDate.Time.Equal(day1) || len(e.fake.heartbeatCalls()) != 0 {
		t.Fatalf("import = %+v, heartbeat calls %v", imp, e.fake.heartbeatCalls())
	}
}

// TestImportCapsEachDayInAWindow applies the daily cap per UTC day inside a
// multi-day window, including when the window is split for size.
func TestImportCapsEachDayInAWindow(t *testing.T) {
	for _, tt := range []struct {
		name  string
		split bool
	}{{"one request", false}, {"split window", true}} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.linkedUser(t, "alice", 42)
			for i := range 20 {
				e.fake.addHeartbeat(42, day1.Add(time.Duration(i)*30*time.Minute), "/one.go", nil)
				e.fake.addHeartbeat(42, day1.Add(24*time.Hour+time.Duration(i)*30*time.Minute), "/two.go", nil)
			}
			e.fake.addHeartbeat(42, day1.Add(50*time.Hour), "/three.go", nil)
			if tt.split {
				e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxHeartbeatsBody = 16 << 10 })
			}
			if _, err := e.importer(t, ImportOptions{MaxPerDay: 10}).Run(context.Background(), alice, false); err != nil {
				t.Fatal(err)
			}
			calls := e.fake.heartbeatCalls()
			if tt.split != (len(calls) > 1) {
				t.Fatalf("heartbeat calls = %v", calls)
			}
			perDay := map[string]int{}
			for _, h := range e.sink.byUser[alice] {
				perDay[h.Timestamp().UTC().Format(time.DateOnly)]++
			}
			if perDay["2025-03-01"] != 10 || perDay["2025-03-02"] != 10 || perDay["2025-03-03"] != 1 {
				t.Fatalf("per day = %v, want 10, 10 and 1", perDay)
			}
			imp, _ := e.st.GetHackatimeImport(context.Background(), alice)
			if imp.LastError != truncatedNote {
				t.Fatalf("import = %+v; want the truncation note", imp)
			}
		})
	}
}

// TestImportCrashMidWindow fails ingest halfway through a multi-day window.
// The cursor stays at the window's start, and the rerun completes it with
// no loss and no duplicates.
func TestImportCrashMidWindow(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	want := e.addSession(42, day1.Add(time.Hour), 10*time.Minute)
	want += e.addSession(42, day1.Add(25*time.Hour), 10*time.Minute)
	e.sink.failOn = 3 // the third chunk of ten: inside day 2
	opts := ImportOptions{ChunkSize: 10}
	if _, err := e.importer(t, opts).Run(ctx, alice, false); !errors.Is(err, errSinkDown) {
		t.Fatalf("Run() err = %v", err)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if !imp.NextDate.Time.Equal(day1) || imp.Status != importPending {
		t.Fatalf("import after crash = %+v; cursor must stay at the window start", imp)
	}
	if res, err := e.importer(t, opts).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("rerun = %+v, %v", res, err)
	}
	if got := e.sink.count(alice); got != want {
		t.Fatalf("imported %d, want %d", got, want)
	}
	handed := 0
	for _, n := range e.sink.chunks {
		handed += n
	}
	if handed != 20+want { // 20 before the crash, then the whole window again
		t.Fatalf("handed %d heartbeats to ingest, want %d", handed, 20+want)
	}
}

// TestImportResumesRunningImport picks up an import the old day-by-day code
// left running mid-2024 with nothing imported yet, without refetching the
// days before its cursor.
func TestImportResumesRunningImport(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	cursor := time.Date(2024, 5, 21, 0, 0, 0, 0, time.UTC)
	if _, err := e.st.Pool.Exec(ctx, `UPDATE hackatime_imports
		SET from_date = '2024-01-01', next_date = $2, status = 'running', heartbeats_imported = 0
		WHERE user_id = $1`, alice, cursor); err != nil {
		t.Fatal(err)
	}
	e.fake.addHeartbeat(42, time.Date(2024, 2, 1, 9, 0, 0, 0, time.UTC), "/before.go", nil)
	want := e.addSession(42, time.Date(2024, 11, 5, 9, 0, 0, 0, time.UTC), 3*time.Hour)
	res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false)
	if err != nil || !res.Done {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if got := e.sink.count(alice); got != want {
		t.Fatalf("imported %d, want %d (nothing before the cursor)", got, want)
	}
	for _, c := range append(e.fake.spanCalls(), e.fake.heartbeatCalls()...) {
		if c < "2024-05-21" {
			t.Fatalf("fetched %s, before the saved cursor", c)
		}
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || imp.HeartbeatsImported != int64(want) {
		t.Fatalf("import = %+v", imp)
	}
}
