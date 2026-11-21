package e2e

import (
	"context"
	"slices"
	"testing"
	"time"
)

// wantNothingSealed checks that no day was sealed, locally or on the server,
// and that the clock disagreement is shown in status.
func (r *rig) wantNothingSealed(why string) {
	r.t.Helper()
	if days := r.sealedDays(); len(days) != 0 {
		r.t.Fatalf("%s: server holds sealed days %v", why, days)
	}
	if v, err := r.db.Get(context.Background(), "crediting.sealed_through"); err != nil || v != "" {
		r.t.Fatalf("%s: sealed_through = %q (%v), want unset", why, v, err)
	}
	if v, err := r.db.Get(context.Background(), "evidence.cursor"); err != nil || v != "" {
		r.t.Fatalf("%s: evidence cursor = %q (%v), want unset", why, v, err)
	}
	if s := r.status(); s.Minutes.Clock == "" {
		r.t.Fatalf("%s: status shows no clock problem", why)
	}
}

// The real time is Nov 20 12:01 and the agent's clock jumps five days
// ahead. A proxy between agent and server strips the Date header, or puts
// a wrong one in the future. The agent must not seal Nov 20 (still running)
// nor skip Nov 21-24 as empty: if it did, correcting the clock would pause
// crediting until Nov 25. It reads the server's time from the response body
// instead, sees the clocks disagree, seals nothing and keeps crediting.
func TestClockAheadBehindProxySealsNothing(t *testing.T) {
	t.Parallel()
	for _, date := range []string{"strip", "future"} {
		t.Run(date, func(t *testing.T) {
			start := minute.Add(time.Minute + 5*time.Second) // Nov 20 12:01:05
			r := newRig(t, start)
			r.present(minute.Add(-5*time.Minute), minute.Add(10*time.Minute))
			r.edit(minute.Add(20*time.Second), "app")
			r.run()

			ahead := 5 * 24 * time.Hour
			r.with(func(r *rig) {
				r.date = date
				r.now = start.Add(time.Minute + ahead)
				r.serverOffset = -ahead
			})
			r.runAny()
			r.runAny()
			r.wantNothingSealed("clock five days ahead")

			// The clock is corrected: crediting goes on at once.
			r.with(func(r *rig) {
				r.now = minute.Add(4*time.Minute + 5*time.Second)
				r.serverOffset = 0
			})
			r.edit(minute.Add(3*time.Minute+10*time.Second), "app")
			r.run()
			if s := r.status(); s.Minutes.Clock != "" {
				t.Fatalf("status clock after the correction = %q", s.Minutes.Clock)
			}
			if _, total := r.serverRows(minute.Add(3 * time.Minute)); total != 60 {
				t.Fatalf("12:03 holds %d s after the clock was corrected, want 60", total)
			}

			// When Nov 20 has really ended, it is sealed (the first run
			// after the jump measures the server's clock again).
			r.set(time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC))
			r.run()
			r.set(time.Date(2026, 11, 21, 1, 1, 5, 0, time.UTC))
			r.run()
			if days := r.sealedDays(); !slices.Equal(days, []string{"2026-11-20"}) {
				t.Fatalf("server days = %v, want [2026-11-20]", days)
			}
			r.check()
		})
	}
}

// The agent's clock is right; the server's is half an hour behind. Nov 20
// has ended plus the grace hour by both, but the clocks disagree, so
// nothing is sealed until they agree again.
func TestServerClockBehindSealsNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t, minute.Add(time.Minute+5*time.Second))
	r.present(minute.Add(-5*time.Minute), minute.Add(40*time.Minute))
	r.edit(minute.Add(20*time.Second), "app")
	r.run()
	r.set(minute.Add(40*time.Minute + 5*time.Second))
	r.run()

	r.with(func(r *rig) {
		r.now = time.Date(2026, 11, 21, 2, 0, 5, 0, time.UTC)
		r.serverOffset = -30 * time.Minute
	})
	r.runAny()
	r.wantNothingSealed("server clock 30 minutes behind")

	r.with(func(r *rig) { r.serverOffset = 0 })
	r.run()
	if days := r.sealedDays(); !slices.Equal(days, []string{"2026-11-20"}) {
		t.Fatalf("server days = %v, want [2026-11-20]", days)
	}
	if s := r.status(); s.Minutes.Clock != "" {
		t.Fatalf("status clock once the clocks agree = %q", s.Minutes.Clock)
	}
	r.check()
}

// The server is unreachable for three days. Nothing is sealed meanwhile and
// no minute is lost: once the server answers (and so the clocks can be
// compared), every minute goes up and the chain moves past the days in
// order. A block seals the rows logged since the previous one (ADR 0017),
// so Nov 20's block holds all four days of rows, and Nov 21-23, with no
// rows logged after it, are skipped as gaps.
func TestServerDownForDaysCatchesUp(t *testing.T) {
	t.Parallel()
	r := newRig(t, minute.Add(time.Minute+5*time.Second))
	r.with(func(r *rig) { r.down = true })
	var minutes []time.Time
	for d := range 4 {
		m := minute.AddDate(0, 0, d)
		minutes = append(minutes, m)
		r.present(m.Add(-5*time.Minute), m.Add(40*time.Minute))
		r.edit(m.Add(20*time.Second), "app")
		r.set(m.Add(time.Minute + 5*time.Second))
		r.runAny()
		r.set(m.Add(40*time.Minute + 5*time.Second))
		r.runAny()
		r.set(m.Add(13*time.Hour + 5*time.Second)) // the next day, past the grace hour
		r.runAny()
		if days := r.sealedDays(); len(days) != 0 {
			t.Fatalf("days sealed while the server was down: %v", days)
		}
		if v, _ := r.db.Get(context.Background(), "crediting.sealed_through"); v != "" {
			t.Fatalf("sealed_through = %q while the server was down", v)
		}
	}

	// Back an hour later, past any backoff.
	r.with(func(r *rig) {
		r.down = false
		r.now = r.now.Add(time.Hour)
	})
	r.run()
	r.set(r.clock().Add(time.Minute))
	r.run()
	for _, m := range minutes {
		if _, total := r.serverRows(m); total != 60 {
			t.Fatalf("%s holds %d s after catching up, want 60", m.Format(time.DateTime), total)
		}
		r.wantServerMatchesEngine(m)
	}
	if days := r.sealedDays(); !slices.Equal(days, []string{"2026-11-20"}) {
		t.Fatalf("server days = %v, want [2026-11-20]", days)
	}
	if through := r.sealedThrough(); !through.Equal(time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("sealed through %s, want the end of Nov 23", through)
	}
	if s := r.status(); s.Minutes.Clock != "" || s.Minutes.Waiting != 0 || s.Evidence.Error != "" {
		t.Fatalf("status after catching up = %+v", s)
	}
	r.check()
}

// stored is the agent's rows for [from, to) as compared with the server's,
// keys included.
func (r *rig) stored(from, to time.Time) []string {
	r.t.Helper()
	rows, err := r.db.StoredMinutes(context.Background(), from, to)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, c := range rows {
		if !isRemoval(c) {
			out = append(out, localText(c))
		}
	}
	return out
}

// Round-4 counterexample 2: the clock is set back an hour, past minutes
// that are already final, and the person keeps working, so new events are
// stamped inside final minutes. Before ADR 0017 the rollback reopened final
// minutes and forgot the cutoffs that made them; the agent then either
// recomputed a minute the server had frozen or stopped uploading.
//
// Now a final minute is never recomputed, whatever the clock says. Minutes
// still open locally but already frozen on the server are recomputed with
// the back-dated events, sent, refused, and replaced with the server's
// rows. Nothing that was stored changes, every upload is answered, and once
// the clock is corrected the day seals with agent, server and replays in
// agreement.
func TestClockSetBackKeepsFinalMinutes(t *testing.T) {
	t.Parallel()
	r := newRig(t, m(1).Add(5*time.Second))
	working := func(project string) func(now time.Time) {
		return func(now time.Time) {
			prev := now.Truncate(time.Minute).Add(-time.Minute)
			r.present(prev, prev.Add(time.Minute))
			r.edit(prev.Add(20*time.Second), project)
		}
	}
	r.present(m(-5), m(0))
	r.edit(m(-1), "app")
	r.runEvery(m(1), m(32), working("app"))
	before := r.stored(m(-60), m(31))
	finalBefore := r.mustStored(m(2))
	if len(finalBefore) == 0 || !finalBefore[0].Final {
		t.Fatalf("+2 = %+v before the rollback, want final rows", finalBefore)
	}

	// The clock goes back an hour for 40 minutes; the server's stays
	// right. Events are stamped by the agent's clock, so they land in
	// minutes that are final (and later in open ones the server froze).
	r.with(func(r *rig) { r.serverOffset = time.Hour })
	for at := m(32); at.Before(m(72)); at = at.Add(time.Minute) {
		now := at.Add(-time.Hour).Add(5 * time.Second)
		r.set(now)
		working("lib")(now)
		r.run()
		if got := r.stored(m(-60), m(31)); !slices.Equal(got, before) {
			t.Fatalf("at %s (agent clock) stored minutes changed:\nnow    %v\nbefore %v", now.Format("15:04"), got, before)
		}
	}
	s := r.status()
	if s.Minutes.Waiting != 0 || s.Minutes.LateIgnored == 0 || s.Minutes.Clock == "" {
		t.Fatalf("status after the rollback = %+v, want nothing waiting, late events counted, the clock shown", s.Minutes)
	}
	// The run at +11 (agent clock) recomputed +5 with the "lib" events, but
	// the server had frozen it: the agent holds the server's rows.
	if engine, _ := r.engineRows(m(5)); slices.Equal(engine, []string{"app 60"}) {
		t.Fatalf("+5 was not recomputed with the back-dated events: engine %v", engine)
	}
	if got, _ := r.serverRows(m(5)); !slices.Equal(got, []string{"app 60"}) {
		t.Fatalf("server +5 = %v, want the value it froze", got)
	}

	// The clock is corrected.
	r.runEvery(m(72), m(75), nil)
	r.finish("2026-11-20")
}

// suspended credits a device without an input sensor (low signal): the
// editor sends a heartbeat while the person reads (no write, same line),
// so those minutes are idle with the project. The last run before the
// machine sleeps is at +2:05; the person reads at +2:10 and saves at +2:40,
// and wakatime-cli queues that save until after the wake.
func suspended(r *rig) {
	r.runEvery(m(-12), m(3), func(now time.Time) {
		prev := now.Truncate(time.Minute).Add(-time.Minute)
		r.heartbeat(prev.Add(10*time.Second), false)
		r.heartbeat(prev.Add(40*time.Second), false)
	})
	r.heartbeat(m(2).Add(10*time.Second), false)
}

// Round-4 counterexample 4: the machine sleeps from +2:30 to +40 and the
// Runner keeps its state (no restart). Before ADR 0017 a suspend the
// monotonic clock didn't count went unnoticed: the agent made +2 final
// before wakatime-cli flushed the save it queued, and a replay with the
// flushed save disagreed with the server.
//
// The Runner notices a wake by the wall clock or the monotonic clock
// (crediting's woke; a test can't build a time whose monotonic reading lags
// its wall reading, so the rig's clock has none and the wall gap is all the
// Runner sees). When it notices, it settles and the save counts. When it
// doesn't (here: no settle period), the save comes too late to count, and
// agent, server and replays still agree, because the value is a function
// of its key, not of when the agent thought it was settled.
func TestSuspendLateSave(t *testing.T) {
	t.Parallel()
	save := m(2).Add(40 * time.Second)
	t.Run("wake noticed", func(t *testing.T) {
		r := newRig(t, m(-12).Add(5*time.Second), withSettle(15*time.Minute))
		suspended(r)
		r.runEvery(m(40), m(41), nil)
		if s := r.status(); s.Minutes.Settling == "" {
			t.Fatal("no settling after the wake")
		}
		r.heartbeat(save, true) // wakatime-cli's queue, flushed after the wake
		r.runEvery(m(41), m(60), nil)
		if got := r.storedCredit(m(2)); got != 60 {
			t.Fatalf("+2 stored %d s, want 60 for the save flushed after the wake", got)
		}
		if s := r.status(); s.Minutes.LateIgnored != 0 {
			t.Fatalf("late events = %d, want 0", s.Minutes.LateIgnored)
		}
		r.finish("2026-11-20")
	})
	t.Run("wake missed", func(t *testing.T) {
		r := newRig(t, m(-12).Add(5*time.Second))
		suspended(r)
		// The first run after the gap measures the server's clock again;
		// the next makes +2 final.
		r.runEvery(m(40), m(42), nil)
		final := r.mustStored(m(2))
		if len(final) != 1 || !final[0].Final || final[0].Credited != 0 {
			t.Fatalf("+2 after the wake = %+v, want one final idle row", final)
		}
		r.heartbeat(save, true)
		r.runEvery(m(42), m(45), nil)
		if got := r.mustStored(m(2)); !slices.Equal(r.stored(m(2), m(3)), []string{localText(final[0])}) {
			t.Fatalf("+2 changed after it was final: %+v", got)
		}
		if s := r.status(); s.Minutes.LateIgnored != 1 {
			t.Fatalf("late events = %d, want 1", s.Minutes.LateIgnored)
		}
		r.finish("2026-11-20")
	})
}
