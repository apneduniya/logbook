package hackatime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/ingest"
)

// The phase tests import 2024-03-03 .. 2025-03-03 (the env clock's day), so
// the recent phase covers 2025-02-02 .. 2025-03-03.
var (
	longFrom   = time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC)
	recentFrom = time.Date(2025, 2, 2, 0, 0, 0, 0, time.UTC)
)

// isRecent reports whether a logged request starts in the recent phase.
// Requests log their start as RFC 3339, which sorts like the time.
func isRecent(r fakeReq) bool { return r.start >= recentFrom.Format(time.RFC3339) }

// addYear gives Hackatime user hid sessions spread over the backfill range
// and three in the recent days, and returns how many heartbeats in all.
func (e *testEnv) addYear(hid int64) (backfill, recent int) {
	for i := range 12 {
		backfill += e.addSession(hid, longFrom.AddDate(0, 0, 10+i*27).Add(9*time.Hour), time.Hour)
	}
	for _, d := range []int{0, 10, 25} {
		recent += e.addSession(hid, recentFrom.AddDate(0, 0, d).Add(10*time.Hour), time.Hour)
	}
	return backfill, recent
}

// ingestImporter builds an importer that stores into the real ingest
// service, so user_minutes (what the dashboard reads) is filled in.
func (e *testEnv) ingestImporter(opts ImportOptions) *Importer {
	opts.Logger = e.logger
	if opts.WindowDelay == 0 {
		opts.WindowDelay = -1
	}
	opts.Now = e.clock
	return NewImporter(e.st, e.box, e.client, ingest.New(e.st, nil, ingest.WithClock(e.clock)), opts)
}

func (e *testEnv) minuteSeconds(t *testing.T, user uuid.UUID, from, to time.Time) int {
	t.Helper()
	return e.count(t, `SELECT coalesce(sum(seconds), 0)::int FROM user_minutes
		WHERE user_id = $1 AND minute >= $2 AND minute < $3`, user, from, to)
}

func describe(reqs []fakeReq) string {
	parts := make([]string, len(reqs))
	for i, r := range reqs {
		parts[i] = fmt.Sprintf("%s %s", r.kind, r.start[:10])
	}
	return strings.Join(parts, ", ")
}

// TestImportRecentDaysFirst checks that every request for the last 30 days
// goes out before any request for older history.
func TestImportRecentDaysFirst(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	e.setImportStart(t, alice, longFrom)
	backfill, recent := e.addYear(42)

	res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false)
	if err != nil || !res.Done {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	reqs := e.fake.requests()
	t.Logf("request order: %s", describe(reqs))
	lastRecent, firstOlder := -1, -1
	for i, r := range reqs {
		switch {
		case isRecent(r):
			lastRecent = i
		case firstOlder < 0:
			firstOlder = i
		}
	}
	if lastRecent < 0 || firstOlder < 0 || firstOlder < lastRecent {
		t.Fatalf("last recent request #%d, first older request #%d; recent days must all come first: %s",
			lastRecent, firstOlder, describe(reqs))
	}
	if got := e.sink.count(alice); got != backfill+recent {
		t.Fatalf("imported %d heartbeats, want %d", got, backfill+recent)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || !imp.RecentDone || imp.NextDate.Time.Format(time.DateOnly) != "2025-03-03" ||
		imp.HeartbeatsImported != int64(backfill+recent) {
		t.Fatalf("import = %+v", imp)
	}
}

// TestImportRecentVisibleWhenBackfillInterrupted fails the backfill on its
// first heartbeats request. The last 30 days are already on the dashboard
// (user_minutes), and the next run fills in the rest without duplicates.
func TestImportRecentVisibleWhenBackfillInterrupted(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	e.setImportStart(t, alice, longFrom)
	backfill, recent := e.addYear(42)
	e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxRetries = -1 })
	var broken atomic.Bool
	broken.Store(true)
	e.fake.setHBHook(func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if broken.Load() && r.URL.Query().Get("start_time") < recentFrom.Format(time.RFC3339) {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		return false
	})

	var se *StatusError
	if _, err := e.ingestImporter(ImportOptions{}).Run(ctx, alice, false); !errors.As(err, &se) {
		t.Fatalf("Run() err = %v, want the backfill's 502", err)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if !imp.RecentDone || imp.Status != importPending || !imp.NextDate.Time.Equal(longFrom) ||
		imp.HeartbeatsImported != int64(recent) {
		t.Fatalf("import after the backfill failed = %+v", imp)
	}
	for _, d := range []int{0, 10, 25} {
		day := recentFrom.AddDate(0, 0, d)
		if s := e.minuteSeconds(t, alice, day, day.AddDate(0, 0, 1)); s < 3500 {
			t.Fatalf("user_minutes for %s = %ds, want about an hour", day.Format(time.DateOnly), s)
		}
	}
	if s := e.minuteSeconds(t, alice, longFrom, recentFrom); s != 0 {
		t.Fatalf("user_minutes before the recent days = %ds, want none yet", s)
	}
	st := decodeStatus(t, e.do(t, http.MethodGet, "/api/v1/hackatime/link", alice).Body.Bytes())
	// 29 recent days are done: today (2025-03-03) hasn't ended, so it is
	// fetched again on the next link.
	if st.Import == nil || !st.Import.RecentDone || st.Import.DaysDone != 29 || st.Import.RecentFrom != "2025-02-02" ||
		st.Import.NextDate != "2024-03-03" {
		t.Fatalf("status import = %+v", st.Import)
	}

	broken.Store(false)
	if res, err := e.ingestImporter(ImportOptions{}).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("resumed Run() = %+v, %v", res, err)
	}
	if n := e.count(t, "SELECT count(*)::int FROM heartbeats WHERE user_id = $1", alice); n != backfill+recent {
		t.Fatalf("stored %d heartbeats, want %d", n, backfill+recent)
	}
	if s := e.minuteSeconds(t, alice, longFrom, recentFrom); s < 12*3500 {
		t.Fatalf("user_minutes before the recent days = %ds after the backfill", s)
	}
	imp, _ = e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || imp.HeartbeatsImported != int64(backfill+recent) {
		t.Fatalf("import = %+v", imp)
	}
}

// TestImportCrashInEachPhase cancels the run (as a crash would) on the
// second heartbeats request of each phase. The rerun resumes from the saved
// cursors, refetches nothing before them, and ends with every heartbeat
// stored once and counted once.
func TestImportCrashInEachPhase(t *testing.T) {
	for _, tt := range []struct {
		name   string
		recent bool
	}{{"recent phase", true}, {"backfill phase", false}} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.linkedUser(t, "alice", 42)
			e.setImportStart(t, alice, longFrom)
			backfill, recent := e.addYear(42)
			// One window per active day, so each phase has several.
			opts := ImportOptions{WindowActivity: time.Nanosecond}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var seen atomic.Int32
			e.fake.setHBHook(func(_ http.ResponseWriter, r *http.Request, _ int) bool {
				inPhase := r.URL.Query().Get("start_time") >= recentFrom.Format(time.RFC3339)
				if inPhase == tt.recent && seen.Add(1) == 2 {
					cancel()
				}
				return false
			})
			if _, err := e.ingestImporter(opts).Run(ctx, alice, false); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() err = %v, want cancelled", err)
			}
			crashed, _ := e.st.GetHackatimeImport(context.Background(), alice)
			if crashed.RecentDone == tt.recent || crashed.Status != importPending {
				t.Fatalf("import after the crash = %+v", crashed)
			}
			before := len(e.fake.requests())

			e.fake.setHBHook(nil)
			if res, err := e.ingestImporter(opts).Run(context.Background(), alice, false); err != nil || !res.Done {
				t.Fatalf("rerun = %+v, %v", res, err)
			}
			cursor := crashed.NextDate.Time
			if tt.recent {
				cursor = crashed.RecentNext.Time
			}
			for _, r := range e.fake.requests()[before:] {
				if isRecent(r) == tt.recent && r.start < cursor.Format(time.RFC3339) {
					t.Fatalf("rerun fetched %s %s, before the saved cursor %s", r.kind, r.start, cursor.Format(time.DateOnly))
				}
			}
			if n := e.count(t, "SELECT count(*)::int FROM heartbeats WHERE user_id = $1", alice); n != backfill+recent {
				t.Fatalf("stored %d heartbeats, want %d", n, backfill+recent)
			}
			imp, _ := e.st.GetHackatimeImport(context.Background(), alice)
			if imp.Status != importDone || imp.HeartbeatsImported != int64(backfill+recent) {
				t.Fatalf("import = %+v; want done with each heartbeat counted once", imp)
			}
		})
	}
}

// TestImportOutOfOrderWindows holds the first window until the second has
// finished and been saved. The cursor must stay at the start of the gap; it
// moves on only when the first window finishes too, and a failed first
// window leaves it there for the next run.
func TestImportOutOfOrderWindows(t *testing.T) {
	for _, tt := range []struct {
		name      string
		firstFail bool
	}{{"first window finishes last", false}, {"first window fails", true}} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t) // range 2025-03-01 .. 2025-03-03, all recent
			alice := e.linkedUser(t, "alice", 42)
			ctx := context.Background()
			n1 := e.addSession(42, day1.Add(time.Hour), 20*time.Minute)
			n2 := e.addSession(42, day1.Add(25*time.Hour), 10*time.Minute)
			e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxRetries = -1 })

			var gap struct {
				sync.Mutex
				err error
			}
			e.fake.setHBHook(func(w http.ResponseWriter, r *http.Request, _ int) bool {
				if r.URL.Query().Get("start_time") != day1.Format(time.RFC3339) {
					return false
				}
				// Wait for the second window's heartbeats to be saved.
				var imp struct {
					next time.Time
					n    int64
				}
				deadline := time.Now().Add(10 * time.Second)
				for imp.n < int64(n2) && time.Now().Before(deadline) {
					got, err := e.st.GetHackatimeImport(ctx, alice)
					if err != nil {
						gap.Lock()
						gap.err = err
						gap.Unlock()
						break
					}
					imp.next, imp.n = got.RecentNext.Time, got.HeartbeatsImported
					time.Sleep(5 * time.Millisecond)
				}
				gap.Lock()
				switch {
				case gap.err != nil:
				case imp.n != int64(n2):
					gap.err = fmt.Errorf("second window not saved while the first was in flight (imported %d)", imp.n)
				case !imp.next.Equal(day1):
					gap.err = fmt.Errorf("cursor moved to %s past the unfinished first window", imp.next.Format(time.DateOnly))
				}
				gap.Unlock()
				if tt.firstFail {
					w.WriteHeader(http.StatusBadGateway)
					return true
				}
				return false
			})

			res, err := e.importer(t, ImportOptions{WindowActivity: time.Nanosecond, Concurrency: 2}).Run(ctx, alice, false)
			gap.Lock()
			if gap.err != nil {
				t.Fatal(gap.err)
			}
			gap.Unlock()
			imp, _ := e.st.GetHackatimeImport(ctx, alice)
			if !tt.firstFail {
				if err != nil || !res.Done {
					t.Fatalf("Run() = %+v, %v", res, err)
				}
				if imp.Status != importDone || imp.RecentNext.Time.Format(time.DateOnly) != "2025-03-03" ||
					imp.HeartbeatsImported != int64(n1+n2) {
					t.Fatalf("import = %+v", imp)
				}
				return
			}
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("Run() err = %v, want the first window's 502", err)
			}
			if !imp.RecentNext.Time.Equal(day1) || !imp.NextDate.Time.Equal(day1) || imp.RecentDone ||
				imp.HeartbeatsImported != int64(n2) {
				t.Fatalf("import = %+v; the cursor must stay before the failed window", imp)
			}
			e.fake.setHBHook(nil)
			if res, err := e.importer(t, ImportOptions{WindowActivity: time.Nanosecond}).Run(ctx, alice, false); err != nil || !res.Done {
				t.Fatalf("rerun = %+v, %v", res, err)
			}
			imp, _ = e.st.GetHackatimeImport(ctx, alice)
			if e.sink.count(alice) != n1+n2 || imp.HeartbeatsImported != int64(n1+n2) {
				t.Fatalf("imported %d, import = %+v; want %d, each counted once", e.sink.count(alice), imp, n1+n2)
			}
		})
	}
}

// TestImportSharedLimiter runs two users' imports at once through one client.
// Every request waits on the shared limiter, so together they never beat its
// token bucket (2 a second, burst 2), and neither import has more than its
// two windows in flight. Time is virtual, so the test doesn't wait.
func TestImportSharedLimiter(t *testing.T) {
	e := newEnv(t)
	const interval, burst = 500 * time.Millisecond, 2
	vc := newVirtualClock()
	start := vc.Now()
	e.client = e.fake.client(t, func(c *ClientConfig) {
		c.Limiter = NewLimiter(LimiterConfig{Interval: interval, Burst: burst, Clock: vc})
	})
	alice := e.linkedUser(t, "alice", 42)
	bob := e.linkedUser(t, "bob", 43)
	want := map[uuid.UUID]int{}
	for _, u := range []struct {
		id  uuid.UUID
		hid int64
	}{{alice, 42}, {bob, 43}} {
		e.setImportStart(t, u.id, longFrom)
		b, r := e.addYear(u.hid)
		want[u.id] = b + r
	}

	opts := ImportOptions{WindowActivity: time.Nanosecond, Concurrency: 2}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, u := range []uuid.UUID{alice, bob} {
		wg.Go(func() {
			res, err := e.importer(t, opts).Run(context.Background(), u, false)
			if err == nil && !res.Done {
				err = errors.New("not done")
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	for u, n := range want {
		if got := e.sink.count(u); got != n {
			t.Fatalf("user %v imported %d, want %d", u, got, n)
		}
	}

	adm := vc.admissions(start)
	if reqs := e.fake.importRequests(); len(adm) != reqs {
		t.Fatalf("limiter let %d requests through, Hackatime saw %d", len(adm), reqs)
	}
	checkBucket(t, adm, burst, interval)
	span := adm[len(adm)-1] - adm[0]
	t.Logf("%d requests from two imports over %v of virtual time (%.2f/s); first: %v",
		len(adm), span, float64(len(adm)-burst)/span.Seconds(), adm[:8])
	for _, hid := range []int64{42, 43} {
		if peak := e.fake.peakInFlight(hid); peak > opts.Concurrency {
			t.Fatalf("user %d had %d requests in flight, want at most %d", hid, peak, opts.Concurrency)
		}
	}
	// Both imports were running at the same time.
	first, last := map[int64]int{}, map[int64]int{}
	for i, r := range e.fake.requests() {
		if _, ok := first[r.user]; !ok {
			first[r.user] = i
		}
		last[r.user] = i
	}
	if first[43] > last[42] || first[42] > last[43] {
		t.Fatalf("imports didn't overlap: alice #%d..#%d, bob #%d..#%d", first[42], last[42], first[43], last[43])
	}
}

// TestImport429PausesAllImports answers one import's request with 429 and
// Retry-After: 600. Every other caller of the same client, another user's
// import and the trust refresher, stops too without sending anything, until
// the pause is over.
func TestImport429PausesAllImports(t *testing.T) {
	e := newEnv(t)
	vc := newVirtualClock()
	start := vc.Now()
	e.client = e.fake.client(t, func(c *ClientConfig) {
		c.Limiter = NewLimiter(LimiterConfig{Interval: 500 * time.Millisecond, MaxPause: 5 * time.Second, Clock: vc})
	})
	alice := e.linkedUser(t, "alice", 42)
	bob := e.linkedUser(t, "bob", 43)
	ctx := context.Background()
	for _, hid := range []int64{42, 43} {
		e.addSession(hid, day1.Add(time.Hour), time.Hour)
	}
	var limited atomic.Bool
	e.fake.setSpansHook(func(w http.ResponseWriter, _ *http.Request) bool {
		if limited.CompareAndSwap(false, true) {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	})

	var rl *RateLimitedError
	if _, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false); !errors.As(err, &rl) || rl.RetryAfter != 10*time.Minute {
		t.Fatalf("alice Run() err = %v, want rate limited for 10m", err)
	}
	sent, paused := e.fake.importRequests(), time.Now()
	res, err := e.importer(t, ImportOptions{}).Run(ctx, bob, false)
	if !errors.As(err, &rl) || rl.RetryAfter != 10*time.Minute || res.Done {
		t.Fatalf("bob Run() = %+v, %v; want rate limited for 10m", res, err)
	}
	tr := NewTrustRefresher(e.st, e.box, e.client, e.logger, nil)
	if err := tr.Refresh(ctx, bob); !errors.As(err, &rl) {
		t.Fatalf("trust Refresh() err = %v, want rate limited", err)
	}
	if got := e.fake.importRequests(); got != sent {
		t.Fatalf("Hackatime got %d more requests during the pause", got-sent)
	}
	// They give up at once, to snooze, rather than retrying in place.
	if d := time.Since(paused); d > 2*time.Second {
		t.Fatalf("callers took %v to report the pause", d)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, bob)
	if imp.Status != importPending || imp.LastError != userMessage(rl) {
		t.Fatalf("bob's import = %+v", imp)
	}

	vc.advance(10 * time.Minute)
	for _, u := range []uuid.UUID{alice, bob} {
		if res, err := e.importer(t, ImportOptions{}).Run(ctx, u, false); err != nil || !res.Done {
			t.Fatalf("Run() after the pause = %+v, %v", res, err)
		}
	}
	adm := vc.admissions(start)
	for _, at := range adm[1:] {
		if at < 10*time.Minute {
			t.Fatalf("admissions %v: a request went out during the pause", adm)
		}
	}
	if adm[1] != 10*time.Minute || adm[2] != 10*time.Minute+500*time.Millisecond {
		t.Fatalf("admissions %v: want requests to resume at the pause's end, at the sustained rate", adm)
	}
}

// TestImportRelinkAfterLongGap relinks three months after a finished import.
// The new last 30 days come first, then the gap since the old import, and
// nothing the old import finished is fetched again.
func TestImportRelinkAfterLongGap(t *testing.T) {
	e := newEnv(t) // clock: 2025-03-03 15:00 UTC
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	want := e.addSession(42, day1.Add(time.Hour), time.Hour)
	if res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("Run() = %+v, %v", res, err)
	}

	want += e.addSession(42, time.Date(2025, 4, 15, 9, 0, 0, 0, time.UTC), time.Hour)
	want += e.addSession(42, time.Date(2025, 5, 25, 9, 0, 0, 0, time.UTC), time.Hour)
	e.advance(90 * 24 * time.Hour) // 2025-06-01 15:00
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("relink = %d %s", rec.Code, rec.Body)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.NextDate.Time.Format(time.DateOnly) != "2025-03-03" || imp.RecentFrom.Time.Format(time.DateOnly) != "2025-05-03" ||
		imp.RecentNext.Time.Format(time.DateOnly) != "2025-05-03" || imp.RecentDone {
		t.Fatalf("import after relink = %+v", imp)
	}
	before := len(e.fake.requests())
	if res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("Run() after relink = %+v, %v", res, err)
	}
	reqs := e.fake.requests()[before:]
	t.Logf("request order after relink: %s", describe(reqs))
	sawOlder := false
	for _, r := range reqs {
		recent := r.start >= "2025-05-03"
		switch {
		case r.start < "2025-03-03":
			t.Fatalf("refetched %s %s, finished before the relink", r.kind, r.start)
		case recent && sawOlder:
			t.Fatalf("recent request %s %s after the backfill started: %s", r.kind, r.start, describe(reqs))
		case !recent:
			sawOlder = true
		}
	}
	if got := e.sink.count(alice); got != want {
		t.Fatalf("imported %d, want %d", got, want)
	}
	imp, _ = e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || imp.NextDate.Time.Format(time.DateOnly) != "2025-06-01" || imp.HeartbeatsImported != int64(want) {
		t.Fatalf("import = %+v", imp)
	}
}
