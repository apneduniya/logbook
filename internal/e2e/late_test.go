package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Late events. Each test runs the agent's real pipeline and engine against
// the real server, lets events arrive late the way they do in practice (the
// hook spool drained at start, wakatime-cli flushing its offline queue) and
// ends with the ADR 0017 checks: the agent's final minutes, the server's
// rows and replays at each minute's key agree, and every sealed block
// re-hashes.

// reading runs the agent once a minute over [from, to) for a device without
// an input sensor (low signal) whose editor sends a heartbeat at :10 and
// :40 of every minute while the person reads (no write, same line), so
// those minutes are idle with the project.
func (r *rig) reading(from, to time.Time) {
	r.runEvery(from, to, func(now time.Time) {
		prev := now.Truncate(time.Minute).Add(-time.Minute)
		r.heartbeat(prev.Add(10*time.Second), false)
		r.heartbeat(prev.Add(40*time.Second), false)
	})
}

// The same long prompt at -4, -2 and 0 with the person watching in the
// terminal; the agent stops at +1:50. While it is down the turn goes on and
// Claude Code's hooks spool its tool calls (+8, +16). The agent starts again
// at +47. The spooled calls belong to the third prompt's turn, so its
// minutes from 0 on are the third repeat of a prompt (a loop cut).
//
// The catch-up run at +48 runs before the startup drain and computes +2 at
// 60 s; it must not upload it, or the server would freeze that value before
// the drained events could correct it. No minute becomes final before the
// drain has finished and the agent has been up for the settle period, so
// the drained calls count, and agent, server and replays agree.
func TestSpoolDrainedAfterRestartCountsBeforeFinality(t *testing.T) {
	t.Parallel()
	r := newRig(t, m(-30).Add(5*time.Second), withSettle(15*time.Minute))
	r.runEvery(m(-30), m(2), func(now time.Time) {
		prev := now.Truncate(time.Minute).Add(-time.Minute)
		for s := prev; s.Before(prev.Add(time.Minute)); s = s.Add(10 * time.Second) {
			if s.Before(m(1).Add(50 * time.Second)) {
				r.store(sample(s, "terminal")) // the person in the terminal until the agent stops
			}
		}
		for _, p := range []int{-4, -2, 0} {
			if m(p).Equal(prev) {
				r.store(ai(m(p).Add(5*time.Second), wire.AIPromptSubmit))
			}
		}
	})
	// The last samples (+1:00..+1:40) arrive before the agent stops.
	r.set(m(1).Add(50 * time.Second))
	for s := m(1); s.Before(m(1).Add(50 * time.Second)); s = s.Add(10 * time.Second) {
		r.store(sample(s, "terminal"))
	}

	// Down from +1:50 to +47. Restart: the daemon drains the spool before
	// crediting makes anything final.
	drained := make(chan struct{})
	r.restart(func(o *crediting.Options) { o.Drained = drained })
	r.set(m(47).Add(5 * time.Second))
	r.run() // the catch-up run before the drain
	if got := r.storedCredit(m(2)); got != 60 {
		t.Fatalf("+2 computed %d s before the drain, want 60 (the turn seems to end at its prompt)", got)
	}
	if rows, _ := r.serverRows(m(2)); len(rows) != 0 {
		t.Fatalf("+2 uploaded (%v) before the spool was drained", rows)
	}
	r.store(ai(m(8), wire.AIToolStart), ai(m(16), wire.AIToolEnd))
	close(drained)
	r.runEvery(m(48), m(70), nil)

	if got := r.storedCredit(m(2)); got != 0 {
		t.Fatalf("+2 stored %d s, want 0: the third repeat's turn ran through it", got)
	}
	if s := r.status(); s.Minutes.Settling != "" || s.Minutes.Waiting != 0 || s.Minutes.LateIgnored != 0 {
		t.Fatalf("status = %+v", s.Minutes)
	}
	r.finish("2026-11-20")
}

// A device without an input sensor: the agent stops at +2; at +2:10 the
// person saves a file, and wakatime-cli queues that heartbeat until the
// agent is back at +47.
func TestOfflineHeartbeatAfterRestart(t *testing.T) {
	t.Parallel()
	// It arrives within the settle period after the restart: it counts.
	t.Run("within settle", func(t *testing.T) {
		r := newRig(t, m(-12).Add(5*time.Second), withSettle(15*time.Minute))
		r.reading(m(-12), m(2))
		r.restart()
		r.runEvery(m(47), m(48), nil)
		if rows := r.mustStored(m(2)); len(rows) != 1 || rows[0].Credited != 0 {
			t.Fatalf("+2 before the flush = %+v, want one idle row for app", rows)
		}
		r.runEvery(m(48), m(70), func(now time.Time) {
			if now.Truncate(time.Minute).Equal(m(48)) {
				r.heartbeat(m(2).Add(10*time.Second), true)
			}
		})
		if got := r.storedCredit(m(2)); got != 60 {
			t.Fatalf("+2 stored %d s, want 60 for the save that arrived within the settle period", got)
		}
		if s := r.status(); s.Minutes.LateIgnored != 0 {
			t.Fatalf("late events = %d, want 0", s.Minutes.LateIgnored)
		}
		r.finish("2026-11-20")
	})

	// It arrives at +75, after +2 was final: it is logged (and sealed as
	// evidence) but changes nothing, and status counts it.
	t.Run("after finality", func(t *testing.T) {
		r := newRig(t, m(-12).Add(5*time.Second), withSettle(15*time.Minute))
		r.reading(m(-12), m(2))
		r.restart()
		r.runEvery(m(47), m(75), nil)
		before := r.stored(m(-12), m(40))
		r.set(m(75))
		r.heartbeat(m(2).Add(10*time.Second), true)
		seq := r.high()
		r.runEvery(m(75), m(80), nil)
		if got := r.storedCredit(m(2)); got != 0 {
			t.Fatalf("+2 stored %d s after a heartbeat that arrived after it was final, want 0", got)
		}
		if after := r.stored(m(-12), m(40)); !slices.Equal(after, before) {
			t.Fatalf("final minutes changed:\nbefore %v\nafter  %v", before, after)
		}
		if k := r.keyOf(m(2)); k.H >= seq {
			t.Fatalf("+2 has key %+v, at or past the late heartbeat's row %d", k, seq)
		}
		s := r.status()
		if s.Minutes.LateIgnored != 1 || !s.Minutes.LateLatest.Equal(m(2).Add(10*time.Second)) {
			t.Fatalf("late events = %d (latest %s), want 1 at +2:10", s.Minutes.LateIgnored, s.Minutes.LateLatest)
		}
		r.finish("2026-11-20")
	})
}

// working runs the agent once a minute over [from, to) with the person at
// the editor, saving project's main.go at :20 of every minute.
func (r *rig) working(from, to time.Time, project string) {
	r.runEvery(from, to, func(now time.Time) {
		prev := now.Truncate(time.Minute).Add(-time.Minute)
		r.present(prev, prev.Add(time.Minute))
		r.edit(prev.Add(20*time.Second), project)
	})
}

// Round-4 counterexample 3: the server already froze a different value.
// The agent uploads 12:00 while it is open; then it is down from 12:05:30
// to 12:40, and the editor's offline queue delivers a save for lib at
// 12:00:40 when it is back. Locally 12:00 is still open, so the agent
// recomputes it with lib and, once settled, makes that final, but the
// server froze the earlier value at 12:30. The server refuses the rewrite
// and returns its rows; the agent adopts them exactly, key included, so
// agent, server and a replay at that key all say what the server holds.
func TestServerFrozeDifferentValue(t *testing.T) {
	t.Parallel()
	t.Run("keyed", func(t *testing.T) {
		r := newRig(t, m(-10).Add(5*time.Second), withSettle(15*time.Minute))
		r.working(m(-10), m(6), "app")
		held := r.serverMinutes(m(0), m(1))
		frozen := r.stored(m(0), m(1))
		key := r.keyOf(m(0))
		if len(held) != 1 || held[0].text() != frozen[0] {
			t.Fatalf("server 12:00 = %+v, agent %v; want the agent's open rows", held, frozen)
		}

		r.restart()
		r.set(m(40).Add(2 * time.Second))
		r.edit(m(0).Add(40*time.Second), "lib") // wakatime-cli's queue
		late := r.high()
		r.runEvery(m(40), m(60), nil)

		if engine, _ := r.engineRows(m(0)); !slices.ContainsFunc(engine, func(s string) bool { return s[:3] == "lib" }) {
			t.Fatalf("the engine's 12:00 = %v, want lib in it", engine)
		}
		if got := r.stored(m(0), m(1)); !slices.Equal(got, frozen) {
			t.Fatalf("agent 12:00 = %v, want the server's %v", got, frozen)
		}
		if k := r.keyOf(m(0)); k != key || k.H >= late {
			t.Fatalf("agent 12:00 key = %+v, want the server's %+v (before row %d)", k, key, late)
		}
		if c := r.finish("2026-11-20"); c.unkeyed != 0 {
			t.Fatalf("%d unkeyed minutes", c.unkeyed)
		}
	})

	// The server's row was stored before ADR 0017, without a key: an older
	// build of this agent uploaded 12:01, and its answer was lost, so the
	// local migration dropped the row as unanswered. The agent recomputes
	// 12:01 while the server is down and sends it once the server is back,
	// final: the server refuses and returns its unkeyed row, which the
	// agent adopts and counts. Such a minute can't be replayed, and a replay
	// says so instead of comparing.
	t.Run("unkeyed", func(t *testing.T) {
		r := newRig(t, m(-10).Add(5*time.Second), withRunner(func(o *crediting.Options) { o.MaxBackoff = time.Minute }))
		if _, err := r.st.Pool.Exec(context.Background(),
			`INSERT INTO credited_minutes (device_id, user_id, minute, project, label, credited, rules_version, updated_at)
			 VALUES ($1, $2, $3, 'app', 'you', 45, $4, $5)`, r.device, r.user, m(1), struggle.RulesVersion, m(2)); err != nil {
			t.Fatal(err)
		}
		r.with(func(r *rig) { r.down = true })
		for at := m(-10); at.Before(m(40)); at = at.Add(time.Minute) {
			r.set(at.Add(5 * time.Second))
			r.present(at.Add(-time.Minute), at)
			r.edit(at.Add(-40*time.Second), "app")
			r.runAny()
		}
		r.with(func(r *rig) { r.down = false })
		for at := m(40); at.Before(m(43)); at = at.Add(time.Minute) {
			r.set(at.Add(5 * time.Second))
			r.runAny() // the first may still be backing off
		}
		r.runEvery(m(43), m(45), nil)
		rows := r.mustStored(m(1))
		if len(rows) != 1 || rows[0].Credited != 45 || !rows[0].Key.IsZero() || !rows[0].Final {
			t.Fatalf("agent 12:01 = %+v, want the server's unkeyed 45 s", rows)
		}
		if s := r.status(); s.Minutes.Unkeyed != 1 {
			t.Fatalf("status unkeyed = %d, want 1", s.Minutes.Unkeyed)
		}
		if c := r.finish("2026-11-20"); c.unkeyed != 1 {
			t.Fatalf("%d unkeyed minutes, want 1", c.unkeyed)
		}
	})
}

// Round-4 counterexample 5: a long spool drain while minutes age past the
// server's finality. The agent stops at +2:30 and starts again at +47; the
// startup drain then takes until +90. While it drains no minute becomes
// final, and minutes the server would freeze before the agent can settle
// are held back. Minutes young enough are still sent while open, so the
// server freezes +40 at +70 with the value the agent had then.
//
// Before ADR 0017 a late event for such a minute changed the agent's value
// after the server froze it, and the two disagreed for good. Now:
//   - +25 was never sent (it was held back from the start), so a save for
//     it that arrives late in the drain still counts when the agent makes
//     +25 final and sends it as a catch-up;
//   - +40 was frozen on the server; a save for it changes the agent's value,
//     but the server refuses the final rewrite and the agent adopts its
//     rows. The save isn't credited (it came too late), and agent, server
//     and replays agree.
func TestLongDrainWhileMinutesFreeze(t *testing.T) {
	t.Parallel()
	r := newRig(t, m(-12).Add(5*time.Second), withSettle(15*time.Minute))
	r.reading(m(-12), m(3))
	drained := make(chan struct{})
	r.restart(func(o *crediting.Options) { o.Drained = drained })
	r.set(m(47))
	for i := 3; i < 47; i++ {
		r.heartbeat(m(i).Add(10*time.Second), false) // the editor's queue, flushed at the restart
	}
	var saves []int64
	r.runEvery(m(47), m(90), func(now time.Time) {
		switch now.Truncate(time.Minute) {
		case m(80):
			r.heartbeat(m(25).Add(30*time.Second), true)
			saves = append(saves, r.high())
		case m(85):
			r.heartbeat(m(40).Add(30*time.Second), true)
			saves = append(saves, r.high())
		}
		if !now.Before(m(70)) {
			if rows, _ := r.serverRows(m(25)); len(rows) != 0 {
				t.Fatalf("+25 was sent (%v) while the spool drained", rows)
			}
		}
	})
	if s := r.status(); s.Minutes.Settling == "" {
		t.Fatal("settled before the drain finished")
	}
	frozen := r.serverMinutes(m(40), m(41))
	if len(frozen) != 1 || frozen[0].Credited != 0 {
		t.Fatalf("server +40 = %+v, want the idle row it froze", frozen)
	}

	close(drained)
	r.runEvery(m(90), m(95), nil)
	if got := r.storedCredit(m(25)); got != 60 {
		t.Fatalf("+25 stored %d s, want 60 for the save drained before it was final", got)
	}
	if got := r.stored(m(40), m(41)); len(got) != 1 || got[0] != frozen[0].text() {
		t.Fatalf("agent +40 = %v, want the server's %s", got, frozen[0].text())
	}
	if k := r.keyOf(m(40)); k.H >= saves[1] {
		t.Fatalf("+40 key %+v includes the save logged as row %d", k, saves[1])
	}
	if k := r.keyOf(m(25)); k.H < saves[0] {
		t.Fatalf("+25 key %+v misses the save logged as row %d", k, saves[0])
	}
	r.finish("2026-11-20")
}

// Round-4 counterexample 7: an event arrives after its day's block is
// sealed and changes the next day's early minutes. The person watches
// Claude Code in the terminal from 23:40. The same long prompt is submitted
// at 23:53 and at 00:22, and the second turn's tool calls run to 00:40. A
// third copy of the prompt, from 23:59:30, was spooled by its hook and only
// drained at 01:00:30, after Nov 20 was sealed at 01:00:05. With it, 00:22
// is the third repeat within the loop window, so its turn is cut; the
// minutes still open (00:32 on) change.
//
// Before ADR 0017 a day's evidence covered events by event time, so the
// late prompt was in no sealed day and the changed minutes couldn't be
// replayed from evidence. Now it is logged after the Nov 20 block, sealed
// in the Nov 21 block, and every minute replays from the blocks alone.
func TestEventAfterBlockSealed(t *testing.T) {
	t.Parallel()
	at := func(h, mi, s int) time.Time { return time.Date(2026, 11, 20, h, mi, s, 0, time.UTC) }
	r := newRig(t, at(23, 40, 5))
	prompts := map[time.Time]bool{at(23, 53, 0): true, at(24, 22, 0): true}
	tools := map[time.Time]wire.AIEventKind{at(24, 26, 0): wire.AIToolStart, at(24, 30, 0): wire.AIToolEnd,
		at(24, 34, 0): wire.AIToolStart, at(24, 38, 0): wire.AIToolEnd, at(24, 40, 0): wire.AITurnEnd}
	terminal := func(now time.Time) {
		prev := now.Truncate(time.Minute).Add(-time.Minute)
		for s := prev; s.Before(prev.Add(time.Minute)); s = s.Add(10 * time.Second) {
			r.store(sample(s, "terminal"))
		}
		if prompts[prev] {
			r.store(ai(prev.Add(5*time.Second), wire.AIPromptSubmit))
		}
		if kind, ok := tools[prev]; ok {
			r.store(ai(prev.Add(5*time.Second), kind))
		}
	}
	r.runEvery(at(23, 40, 0), at(25, 1, 0), terminal)
	if days := r.sealedDays(); !slices.Equal(days, []string{"2026-11-20"}) {
		t.Fatalf("sealed days = %v, want Nov 20", days)
	}
	nov20, _, err := r.db.EvidenceDayOf(context.Background(), r.device.String(), "2026-11-20")
	if err != nil {
		t.Fatal(err)
	}

	r.set(at(25, 0, 30))
	r.store(ai(at(23, 59, 30), wire.AIPromptSubmit)) // the spool, drained late
	late := r.high()
	r.runEvery(at(25, 1, 0), at(25, 15, 0), terminal)
	if s := r.status(); s.Minutes.LateIgnored == 0 {
		t.Fatal("the late prompt's own minute (23:59, final) was not counted as late")
	}

	changed := 0
	for mm := at(24, 32, 0); mm.Before(at(24, 41, 0)); mm = mm.Add(time.Minute) {
		key := r.keyOf(mm)
		if key.H < late {
			continue
		}
		without, err := crediting.ReplayMinute(context.Background(), r.db, mm, localdb.Key{T: key.T, H: nov20.SeqTo}, nil, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if creditText(without) != creditText(r.mustStored(mm)) {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("the late prompt changed no minute of Nov 21")
	}
	if late <= nov20.SeqTo {
		t.Fatalf("the late prompt is row %d, inside the sealed Nov 20 block (to %d)", late, nov20.SeqTo)
	}
	r.finish("2026-11-21")
}
