package hackatime

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestImportMidDayThenRelink links in the middle of a day. The rest of that
// day must still be imported on a later link, not lost.
func TestImportMidDayThenRelink(t *testing.T) {
	e := newEnv(t) // clock: 2025-03-03 15:00 UTC
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	today := time.Date(2025, 3, 3, 0, 0, 0, 0, time.UTC)
	e.fake.addHeartbeat(42, today.Add(9*time.Hour), "/morning.go", nil)

	if res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || !imp.NextDate.Time.Equal(today) {
		t.Fatalf("import = %+v; cursor must stay on the unfinished day", imp)
	}
	s := decodeStatus(t, e.do(t, http.MethodGet, "/api/v1/hackatime/link", alice).Body.Bytes())
	if s.Import == nil || s.Import.DaysDone != s.Import.DaysTotal {
		t.Fatalf("status import = %+v; a done import shows full progress", s.Import)
	}

	// The user keeps coding on Hackatime that evening, then relinks next day.
	e.fake.addHeartbeat(42, today.Add(20*time.Hour), "/evening.go", nil)
	e.advance(19 * time.Hour) // 2025-03-04 10:00
	if rec := e.link(t, alice, 42); rec.Code != http.StatusSeeOther {
		t.Fatalf("relink = %d %s", rec.Code, rec.Body)
	}
	if res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("Run() after relink = %+v, %v", res, err)
	}
	if got := e.sink.count(alice); got != 2 {
		t.Fatalf("imported %d heartbeats, want morning and evening", got)
	}
	imp, _ = e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || !imp.NextDate.Time.Equal(today.AddDate(0, 0, 1)) || imp.HeartbeatsImported != 2 {
		t.Fatalf("import = %+v", imp)
	}
}

// TestImportRunSupersededByRelink unlinks and relinks while a run is in
// flight. The old run's revoked token must not mark the new import failed.
func TestImportRunSupersededByRelink(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	for d := range 3 {
		e.fake.addHeartbeat(42, day1.Add(time.Duration(d)*24*time.Hour+time.Hour), "/x.go", nil)
	}
	e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxRetries = -1 })
	var fired atomic.Bool
	relinked := make(chan error, 1)
	e.fake.setHBHook(func(_ http.ResponseWriter, _ *http.Request, _ int) bool {
		if fired.CompareAndSwap(false, true) {
			// Unlink revokes the token this request carries; relink gets a new one.
			err := e.svc.Unlink(ctx, alice)
			if err == nil {
				err = e.linkDirect(ctx, alice, 42)
			}
			relinked <- err
		}
		return false // answer normally: 401 for the revoked token
	})

	res, runErr := e.importer(t, ImportOptions{}).Run(ctx, alice, false)
	if err := <-relinked; err != nil {
		t.Fatalf("unlink/relink: %v", err)
	}
	if runErr != nil || res.Done {
		t.Fatalf("superseded Run() = %+v, %v; want not done and no error, so it runs again", res, runErr)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importPending || imp.LastError != "" || !imp.NextDate.Time.Equal(day1) {
		t.Fatalf("new import = %+v; the old run must not touch it", imp)
	}

	if res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("fresh Run() = %+v, %v", res, err)
	}
	if e.sink.count(alice) != 3 {
		t.Fatalf("imported %d, want 3", e.sink.count(alice))
	}
}

// TestImportFinishRespectsExtension relinks the same account while the last
// day is being fetched, extending to_date. The import must stay runnable
// rather than be marked done without the extra day.
func TestImportFinishRespectsExtension(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	ctx := context.Background()
	var fired atomic.Bool
	relinked := make(chan error, 1)
	e.fake.setHBHook(func(_ http.ResponseWriter, r *http.Request, _ int) bool {
		// The window holding the last day ends just before 2025-03-04.
		if strings.HasPrefix(r.URL.Query().Get("end_time"), "2025-03-03T23:59:59") && fired.CompareAndSwap(false, true) {
			e.advance(24 * time.Hour)
			relinked <- e.linkDirect(ctx, alice, 42)
		}
		return false
	})
	res, runErr := e.importer(t, ImportOptions{}).Run(ctx, alice, false)
	if err := <-relinked; err != nil {
		t.Fatalf("relink: %v", err)
	}
	if runErr != nil || res.Done {
		t.Fatalf("Run() = %+v, %v; want superseded", res, runErr)
	}
	imp, _ := e.st.GetHackatimeImport(ctx, alice)
	if imp.Status == importDone || imp.ToDate.Time.Format(time.DateOnly) != "2025-03-04" {
		t.Fatalf("import = %+v; the extension must not be marked done", imp)
	}
	if res, err := e.importer(t, ImportOptions{}).Run(ctx, alice, false); err != nil || !res.Done {
		t.Fatalf("second Run() = %+v, %v", res, err)
	}
	imp, _ = e.st.GetHackatimeImport(ctx, alice)
	if imp.Status != importDone || imp.NextDate.Time.Format(time.DateOnly) != "2025-03-04" {
		t.Fatalf("import after second run = %+v", imp)
	}
}

// TestImportCapsDay bounds what one day may bring in: heartbeats past the
// daily cap, and windows still too large at the minimum size, are skipped
// and the user is told.
func TestImportCapsDay(t *testing.T) {
	t.Run("daily cap", func(t *testing.T) {
		e := newEnv(t)
		alice := e.linkedUser(t, "alice", 42)
		for i := range 30 {
			e.fake.addHeartbeat(42, day1.Add(time.Duration(i)*time.Minute), "/f.go", nil)
		}
		e.fake.addHeartbeat(42, day1.Add(25*time.Hour), "/day2.go", nil)
		if _, err := e.importer(t, ImportOptions{MaxPerDay: 10}).Run(context.Background(), alice, false); err != nil {
			t.Fatal(err)
		}
		if got := e.sink.count(alice); got != 11 {
			t.Fatalf("imported %d, want 10 from day 1 and 1 from day 2", got)
		}
		imp, _ := e.st.GetHackatimeImport(context.Background(), alice)
		if imp.Status != importDone || imp.LastError != truncatedNote {
			t.Fatalf("import = %+v; want done with the truncation note kept", imp)
		}
	})
	t.Run("oversized minimum window", func(t *testing.T) {
		e := newEnv(t)
		alice := e.linkedUser(t, "alice", 42)
		// 200 heartbeats inside one 15-minute window, plus one at noon.
		for i := range 200 {
			e.fake.addHeartbeat(42, day1.Add(time.Duration(i)*time.Second), "/burst.go", nil)
		}
		e.fake.addHeartbeat(42, day1.Add(12*time.Hour), "/noon.go", nil)
		e.client = e.fake.client(t, func(c *ClientConfig) { c.MaxHeartbeatsBody = 16 << 10 })
		if _, err := e.importer(t, ImportOptions{}).Run(context.Background(), alice, false); err != nil {
			t.Fatal(err)
		}
		if got := e.sink.count(alice); got != 1 {
			t.Fatalf("imported %d, want only the noon heartbeat", got)
		}
		imp, _ := e.st.GetHackatimeImport(context.Background(), alice)
		if imp.LastError != truncatedNote {
			t.Fatalf("import = %+v; want the truncation note", imp)
		}
	})
}

func TestImportStalledUsers(t *testing.T) {
	e := newEnv(t)
	alice := e.linkedUser(t, "alice", 42)
	bob := e.linkedUser(t, "bob", 43)
	carol := e.linkedUser(t, "carol", 44)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	for _, u := range []uuid.UUID{alice, carol} {
		if _, err := e.st.Pool.Exec(ctx, "UPDATE hackatime_imports SET updated_at = $2 WHERE user_id = $1", u, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.Pool.Exec(ctx, "UPDATE hackatime_imports SET status = 'done' WHERE user_id = $1", carol); err != nil {
		t.Fatal(err)
	}
	// updated_at is written by the database clock, so use real time here.
	im := NewImporter(e.st, e.box, e.client, e.sink, ImportOptions{Logger: e.logger})
	ids, err := im.StalledUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != alice {
		t.Fatalf("StalledUsers() = %v, want only alice (not fresh bob %v or done carol %v)", ids, bob, carol)
	}
}
