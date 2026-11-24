package crediting

import (
	"context"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// slowSealing makes every run hash at most one step of a block, so sealing
// the 19th of addDay (two hours with rows) takes three runs.
func slowSealing(o *Options) { o.SealRows = 1 }

// reserved starts sealing the 19th: the device registers on the 19th, the
// day gets addDay's rows, and the first run after its grace hour freezes
// its bound and hashes one step. It returns the reservation.
func reserved(t *testing.T, e *env) localdb.Sealing {
	t.Helper()
	e.run() // registers the device on the 19th
	addDay(t, e.db, "2026-11-19")
	e.set(time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC))
	e.run()
	s, ok, err := e.db.Sealing(context.Background())
	if err != nil || !ok || s.Day != "2026-11-19" || s.SeqFrom != 0 || s.SeqTo != 3 || s.Cursor == 0 {
		t.Fatalf("sealing = %+v, %v, %v; want the 19th part hashed", s, ok, err)
	}
	if st := e.status(); st.Evidence.Sealing != "2026-11-19" {
		t.Fatalf("status = %+v, want the 19th shown as being sealed", st.Evidence)
	}
	if days := e.srv.storedDays(e.deviceID()); len(days) != 0 {
		t.Fatalf("server days = %+v before the block is hashed", days)
	}
	return s
}

// untilSealed runs, a minute apart, until the server holds day.
func untilSealed(t *testing.T, e *env, day string) storedDay {
	t.Helper()
	for range 10 {
		e.advance(time.Minute)
		e.run()
		for _, d := range e.srv.storedDays(e.deviceID()) {
			if d.Day == day {
				return d
			}
		}
	}
	t.Fatalf("%s not sealed; status %+v", day, e.status().Evidence)
	return storedDay{}
}

// A row logged while a block is being hashed is after its frozen bound: it
// goes into the next block, not lost between the two.
func TestLateRowDuringHashGoesToNextBlock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	e.options = slowSealing
	e.open()
	s := reserved(t, e)
	// A late sample for the day being hashed, in an hour not hashed yet.
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity",
		Activity: &wire.ActivitySample{Time: time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC), FocusKind: "editor"}}}); err != nil {
		t.Fatal(err)
	}
	day19 := untilSealed(t, e, "2026-11-19")
	if day19.SeqTo != s.SeqTo || day19.EventCounts[evidence.TypeActivity] != 1 {
		t.Fatalf("the 19th = %+v; want its frozen bound %d and one sample", day19, s.SeqTo)
	}
	if st := e.status(); st.Evidence.Sealing != "" || st.Evidence.LastDay != "2026-11-19" {
		t.Fatalf("status = %+v", st.Evidence)
	}
	local, _, _ := e.db.EvidenceDayOf(ctx, e.deviceID(), "2026-11-19")
	if got := e.replayHash(local); got != day19.ContentHash {
		t.Fatalf("one-shot hash of the block = %s, signed %s", got, day19.ContentHash)
	}

	addDay(t, e.db, "2026-11-20")
	e.set(time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC))
	day20 := untilSealed(t, e, "2026-11-20")
	next, _, _ := e.db.EvidenceDayOf(ctx, e.deviceID(), "2026-11-20")
	rows, err := ReplayBlock(ctx, e.db, next)
	if err != nil {
		t.Fatal(err)
	}
	late := false
	for _, r := range rows {
		late = late || r.Seq == s.SeqTo+1 && r.Time.Equal(time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC))
	}
	if day20.SeqFrom != s.SeqTo || !late || day20.EventCounts[evidence.TypeActivity] != 2 {
		t.Fatalf("the 20th = %+v; want it to start at %d with the late sample", day20, s.SeqTo)
	}
}

// Once a day's bound is frozen, no minute of it is computed again, even
// with the clock set back into it while the block is still being hashed: a
// minute computed then would have a key past the bound, which its block
// couldn't replay.
func TestClockSetBackWhileHashing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	e.options = slowSealing
	e.open()
	s := reserved(t, e)
	never := time.Date(2026, 11, 19, 23, 50, 0, 0, time.UTC) // no run computed it
	e.credit(never, "app", 60)
	calls := e.computeCalls()
	e.set(time.Date(2026, 11, 19, 23, 55, 5, 0, time.UTC))
	e.run()
	e.open() // a restart doesn't forget it either
	e.run()
	if n := e.computeCalls(); n != calls {
		t.Fatalf("the engine ran %d times with the clock back in the day being sealed", n-calls)
	}
	if st := e.status(); st.Minutes.Clock == "" {
		t.Fatalf("status = %+v, want the clock shown", st.Minutes)
	}

	e.set(time.Date(2026, 11, 20, 1, 5, 5, 0, time.UTC))
	untilSealed(t, e, "2026-11-19")
	rows, err := e.db.StoredMinutes(ctx, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Minute.Equal(never) || r.Key.H > s.SeqTo {
			t.Fatalf("minute %s of the sealed day stored with key H %d (bound %d)", r.Minute.Format("15:04"), r.Key.H, s.SeqTo)
		}
	}
	if m, ok := e.srv.minute(e.deviceID(), never, "app"); ok {
		t.Fatalf("server holds %+v", m)
	}
}

// Hashing resumes after a restart from the saved hasher state and bucket
// list, and the block hashes as a whole to the same root.
func TestRestartMidHashResumes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	e.options = slowSealing
	e.open()
	first := reserved(t, e)
	e.open()
	e.advance(time.Minute)
	e.run()
	s, ok, err := e.db.Sealing(ctx)
	if err != nil || !ok || s.Cursor <= first.Cursor || s.Buckets <= first.Buckets {
		t.Fatalf("after a restart, sealing = %+v (was %+v), %v, %v; want it to go on", s, first, ok, err)
	}
	e.open()
	day := untilSealed(t, e, "2026-11-19")
	local, _, _ := e.db.EvidenceDayOf(ctx, e.deviceID(), "2026-11-19")
	if local.Buckets != day.Buckets || day.Buckets != 4 || e.replayHash(local) != day.ContentHash {
		t.Fatalf("block = %+v, server %+v", local, day)
	}
}

// A device that takes over (ADR 0017 point 5) starts its chain at the log's
// high-water mark, with the repo mapping logged again as its first rows,
// and drops the block the previous device was still hashing.
func TestTakeoverStartsChainAtBaseline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	e.options = slowSealing
	e.open()
	if err := e.db.RecordRepos(ctx, e.clock(), []struggle.Repo{{Path: "/src/app", Project: "app"}}); err != nil {
		t.Fatal(err)
	}
	e.run()
	old := e.deviceID()
	addDay(t, e.db, "2026-11-19")
	e.set(time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC))
	e.run()
	if _, ok, _ := e.db.Sealing(ctx); !ok {
		t.Fatal("the 19th isn't being sealed")
	}

	// Another account: the next minute upload finds the device unknown
	// (before the block is hashed further), and the run after registers a
	// new device.
	e.credit(time.Date(2026, 11, 20, 0, 59, 0, 0, time.UTC), "app", 60)
	e.setKey("key-2")
	for range 2 {
		e.advance(2 * time.Second)
		e.run()
	}
	id := e.deviceID()
	if id == old {
		t.Fatal("no new device")
	}
	if _, ok, _ := e.db.Sealing(ctx); ok {
		t.Fatal("the previous device's block is still being sealed")
	}
	if st := e.status(); st.Evidence.Sealing != "" {
		t.Fatalf("status = %+v", st.Evidence)
	}
	base, err := e.db.ChainBaseline(ctx, id)
	// The 19th's snapshot (seq 5) and the takeover's (seq 6) follow its
	// four rows.
	if err != nil || base != 5 {
		t.Fatalf("baseline = %d, %v; want 5", base, err)
	}

	addDay(t, e.db, "2026-11-20")
	e.set(time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC))
	day := untilSealed(t, e, "2026-11-20")
	if day.SeqFrom != base || day.PrevChainHash != "" {
		t.Fatalf("first block of the new device = %+v; want it to start at %d", day, base)
	}
	local, _, _ := e.db.EvidenceDayOf(ctx, id, "2026-11-20")
	rows, err := ReplayBlock(ctx, e.db, local)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Seq <= base {
		t.Fatalf("first block holds row %d at or below its baseline", rows[0].Seq)
	}
	if evidence.Counts(rows)[evidence.TypeRepo] != 1 || local.EventCounts[evidence.TypeRepo] != 1 {
		t.Fatalf("first block counts = %v; want the takeover's snapshot row", local.EventCounts)
	}
}
