package e2e

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
)

// An edit at 12:00, then the clock jumps five days ahead and back. The
// minutes after the edit must still get the thinking grace from it: the
// in-memory window must hold the 12:00 events again, not just what was
// logged after the jump.
func TestClockJumpKeepsHistory(t *testing.T) {
	t.Parallel()
	r := newRig(t, minute.Add(time.Minute+5*time.Second))
	r.present(minute.Add(-5*time.Minute), minute.Add(10*time.Minute))
	r.edit(minute.Add(5*time.Second), "app")
	r.run()

	ahead := 5 * 24 * time.Hour
	r.with(func(r *rig) {
		r.now = minute.Add(2*time.Minute + 5*time.Second + ahead)
		r.serverOffset = -ahead
	})
	r.runAny()

	r.with(func(r *rig) {
		r.now = minute.Add(5*time.Minute + 5*time.Second)
		r.serverOffset = 0
	})
	r.run()
	got, total := r.serverRows(minute.Add(2 * time.Minute))
	if total != 60 || !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "app ") }) {
		t.Fatalf("12:02 holds %v (%d s), want 60 s of thinking grace for app", got, total)
	}
	r.wantServerMatchesEngine(minute.Add(2 * time.Minute))
	r.finish("2026-11-20")
}

// samplesIn is the times of activity samples in the engine's last input.
func (r *rig) samplesIn() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Time, len(r.input.Activity))
	for i, s := range r.input.Activity {
		out[i] = s.Time
	}
	return out
}

// coldSamples is the times of the activity samples a cold read of the log
// gives for the window of a run at now.
func (r *rig) coldSamples(now time.Time) []time.Time {
	r.t.Helper()
	to := now.Truncate(time.Minute)
	w, err := r.db.ReadWindow(context.Background(), to.Add(-crediting.Window-crediting.History), to, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	out := make([]time.Time, len(w.Events.Activity))
	for i, s := range w.Events.Activity {
		out[i] = s.Event.Time
	}
	return out
}

// Round-4 counterexample 8: the deletion generation changes during a cache
// refresh. The agent works from Nov 20 23:00 and seals Nov 20 at 01:00:05.
// At 01:21 another process prunes sealed samples from 23:25 to 23:45: rows
// the run's event cache holds (its window and history start at 23:21) but
// no open minute reads (they are 00:53 on, and the engine reads at most an
// hour back). The prune has begun when the run takes its snapshot and
// commits before the run stores its result.
//
// Before ADR 0017 the cache read the generation outside the refresh it
// guarded, so a prune could slip between the two and the cache kept rows
// the table no longer had. Now the generation is read in the run's
// snapshot: that run uses exactly the snapshot's rows (the pruned ones
// included, as they were still there), and the next run sees the new
// generation and drops them. Minutes are then replayed from the sealed
// blocks, which still hold the pruned rows.
func TestCacheSeesPruneInItsSnapshot(t *testing.T) {
	t.Parallel()
	at := func(h, mi int) time.Time { return time.Date(2026, 11, 20, h, mi, 0, 0, time.UTC) }
	r := newRig(t, at(23, 0).Add(5*time.Second))
	r.present(at(22, 50), at(23, 0))
	r.working(at(23, 0), at(25, 21), "app")
	if days := r.sealedDays(); !slices.Equal(days, []string{"2026-11-20"}) {
		t.Fatalf("sealed days = %v, want Nov 20", days)
	}

	exec := r.rawConn()
	now := at(25, 21).Add(5 * time.Second)
	var snapshot []time.Time
	r.with(func(rr *rig) {
		rr.beforeSnapshot = func() {
			snapshot = r.coldSamples(now)
			exec("BEGIN IMMEDIATE")
			exec(`DELETE FROM activity_samples WHERE time >= ? AND time < ?
				AND seq <= (SELECT CAST(value AS INTEGER) FROM kv WHERE key = 'log.sealed')`,
				at(23, 25).UnixMilli(), at(23, 45).UnixMilli())
			exec(`INSERT INTO kv (key, value, updated_at) VALUES ('events.generation', '1', 0)
				ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(kv.value AS INTEGER) + 1 AS TEXT)`)
		}
		rr.afterSnapshot = func() { exec("COMMIT") }
	})
	r.pruned = true
	r.set(now)
	r.present(at(25, 20), at(25, 21))
	r.edit(at(25, 20).Add(20*time.Second), "app")
	r.run()
	if got := r.samplesIn(); !slices.Equal(got, snapshot) {
		t.Fatalf("the run read %d samples, its snapshot held %d", len(got), len(snapshot))
	}
	pruned := r.coldSamples(now)
	if len(pruned) != len(snapshot)-120 {
		t.Fatalf("the prune left %d of %d samples, want 120 fewer", len(pruned), len(snapshot))
	}

	r.working(at(25, 22), at(25, 23), "app")
	if got, want := r.samplesIn(), r.coldSamples(at(25, 22)); !slices.Equal(got, want) {
		t.Fatalf("after the prune the run read %d samples, the log holds %d", len(got), len(want))
	}
	nov20, _, err := r.db.EvidenceDayOf(context.Background(), r.device.String(), "2026-11-20")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crediting.ReplayBlock(context.Background(), r.db, nov20); !errors.Is(err, crediting.ErrNotReproducible) {
		t.Fatalf("replaying the pruned Nov 20 block from the log: %v, want ErrNotReproducible", err)
	}
	r.finish("2026-11-21")
}
