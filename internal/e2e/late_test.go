package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Late arrivals (ADR 0016). Each test runs the agent's real pipeline and
// engine against the real server, stops the agent, lets events arrive late
// the way they do in practice (the hook spool drained at start,
// wakatime-cli flushing its offline queue), and checks that the minutes the
// agent made final, the server's copy and a replay of the raw events with
// the persisted cutoffs all agree.

// m is minute + i minutes.
func m(i int) time.Time { return minute.Add(time.Duration(i) * time.Minute) }

// pinned stores events with their arrival pinned to at.
func (r *rig) pinned(at time.Time, fn func()) {
	r.with(func(r *rig) { r.arrived = at })
	defer r.with(func(r *rig) { r.arrived = time.Time{} })
	fn()
}

// store stores raw local events (as the loopback API or the spool drainer
// does), arriving now (see arrival).
func (r *rig) store(evs ...wire.LocalEvent) {
	r.t.Helper()
	if err := r.db.InsertEvents(context.Background(), evs); err != nil {
		r.t.Fatal(err)
	}
}

// heartbeat stores one editor heartbeat for app's main.go at line 10.
func (r *rig) heartbeat(t time.Time, write bool) {
	r.t.Helper()
	h := wire.Heartbeat{Entity: "/src/app/main.go", Project: "app", IsWrite: write, Lineno: new(10), Cursorpos: new(1),
		Time: float64(t.UnixMilli()) / 1000}
	if err := r.db.Enqueue(context.Background(), []wire.Heartbeat{h}, false); err != nil {
		r.t.Fatal(err)
	}
}

func ai(t time.Time, kind wire.AIEventKind) wire.LocalEvent {
	e := wire.AIEvent{Time: t, Harness: wire.HarnessClaudeCode, Kind: kind, SessionID: "cc", Project: "app", Cwd: "/src/app"}
	if kind == wire.AIPromptSubmit {
		e.PromptChars, e.PromptHash = 60, strings.Repeat("ab", 32) // the same long prompt every time
	}
	return wire.LocalEvent{Type: "ai", AI: &e}
}

func sample(t time.Time) wire.LocalEvent {
	return wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{Time: t, IdleSeconds: 1, FocusKind: "terminal"}}
}

// runEvery runs the agent once a minute, 5 s past it, for minutes [from, to),
// calling before (if not nil) ahead of each run.
func (r *rig) runEvery(from, to time.Time, before func(now time.Time)) {
	r.t.Helper()
	for at := from; at.Before(to); at = at.Add(time.Minute) {
		now := at.Add(5 * time.Second)
		r.set(now)
		if before != nil {
			before(now)
		}
		r.run()
	}
}

// credit is one stored or replayed row as compared: what the server checks
// in a re-run (label, project, credit, cuts).
func credit(ms []localdb.CreditedMinute) []string {
	var out []string
	for _, c := range ms {
		if c.Label == string(struggle.LabelIdle) && c.Credited == 0 && len(c.Cuts) == 0 && len(c.Flags) == 0 {
			continue // a removal sent to the server; the engine never emits it
		}
		out = append(out, fmt.Sprintf("%s %s %s %d %v", c.Minute.Format("15:04"), c.Project, c.Label, c.Credited, c.Cuts))
	}
	return out
}

// wantFinalMatchesReplay checks that every final minute in [from, to)
// stored by the agent equals a replay of the raw events with the persisted
// cutoffs, and returns the stored rows.
func (r *rig) wantFinalMatchesReplay(from, to time.Time) []localdb.CreditedMinute {
	r.t.Helper()
	ctx := context.Background()
	ft, err := r.db.FinalThrough(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	if ft.Before(to) {
		r.t.Fatalf("minutes up to %s are not final yet (final through %s)", to.Format("15:04"), ft.Format("15:04"))
	}
	stored, err := r.db.StoredMinutes(ctx, from, to)
	if err != nil {
		r.t.Fatal(err)
	}
	journal, err := r.db.RepoMap(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	cutoffs, err := r.db.CutoffsBetween(ctx, from, to)
	if err != nil {
		r.t.Fatal(err)
	}
	replayed, err := crediting.ReplayMinutes(ctx, r.db, journal, cutoffs, from, to, struggle.Compute, time.UTC)
	if err != nil {
		r.t.Fatal(err)
	}
	if got, want := credit(stored), credit(replayed); !slices.Equal(got, want) {
		r.t.Fatalf("stored final minutes differ from a replay with the persisted cutoffs %v:\nstored %v\nreplay %v", cutoffs, got, want)
	}
	return stored
}

// wantServerMatchesStored checks that the server holds exactly the agent's
// stored rows for minute at.
func (r *rig) wantServerMatchesStored(at time.Time) {
	r.t.Helper()
	stored, err := r.db.StoredMinutes(context.Background(), at, at.Add(time.Minute))
	if err != nil {
		r.t.Fatal(err)
	}
	var want []string
	total := 0
	for _, s := range stored {
		if s.Label == string(struggle.LabelIdle) && s.Credited == 0 && len(s.Cuts) == 0 && len(s.Flags) == 0 {
			continue // a removal: the server deleted the row
		}
		want = append(want, fmt.Sprintf("%s %d", s.Project, s.Credited))
		total += s.Credited
	}
	got, gotTotal := r.serverRows(at)
	if !slices.Equal(got, want) || gotTotal != total {
		r.t.Fatalf("server %s = %v, agent stored %v", at.Format("15:04"), got, want)
	}
}

// mustStored is the agent's stored rows for minute at.
func (r *rig) mustStored(at time.Time) []localdb.CreditedMinute {
	r.t.Helper()
	stored, err := r.db.StoredMinutes(context.Background(), at, at.Add(time.Minute))
	if err != nil {
		r.t.Fatal(err)
	}
	return stored
}

// storedCredit is the agent's stored credit for minute at.
func (r *rig) storedCredit(at time.Time) int {
	r.t.Helper()
	total := 0
	for _, s := range r.mustStored(at) {
		total += s.Credited
	}
	return total
}

// The same long prompt at -4, -2 and 0 with the person watching in the
// terminal; the agent stops at +1:50. While it is down the turn goes on and
// Claude Code's hooks spool its tool calls (+8, +16). The agent starts again
// at +47. The spooled calls belong to the third prompt's turn, so its
// minutes from 0 on are the third repeat of a prompt (a loop cut) whatever
// moment they are computed at.
//
// Before ADR 0016 the catch-up run at +48 could run before the startup
// drain: it froze +2 at 60 s and uploaded it, and a replay with the drained
// events (the server's step-5 check) cut it to 0. Now no minute becomes
// final before the drain has finished and the agent has been up for the
// settle period, and a replay uses the cutoff the agent used, so the agent,
// the server and a replay agree.
func TestSpoolDrainedAfterRestartCountsBeforeFinality(t *testing.T) {
	settle := func(o *crediting.Options) { o.Settle = 15 * time.Minute }
	r := newRig(t, m(-30).Add(5*time.Second), settle)
	r.runEvery(m(-30), m(2), func(now time.Time) {
		prev := now.Truncate(time.Minute).Add(-time.Minute)
		for s := prev; s.Before(prev.Add(time.Minute)); s = s.Add(10 * time.Second) {
			if s.Before(m(1).Add(50 * time.Second)) {
				r.store(sample(s)) // the person in the terminal until the agent stops
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
		r.store(sample(s))
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
	r.pinned(m(47).Add(10*time.Second), func() {
		r.store(ai(m(8), wire.AIToolStart), ai(m(16), wire.AIToolEnd))
	})
	close(drained)
	r.runEvery(m(48), m(70), nil)

	if got := r.storedCredit(m(2)); got != 0 {
		t.Fatalf("+2 stored %d s, want 0: the third repeat's turn ran through it", got)
	}
	r.wantFinalMatchesReplay(m(-30), m(40))
	for i := -6; i <= 30; i++ {
		r.wantServerMatchesStored(m(i))
	}
	if s := r.status(); s.Minutes.Settling != "" || s.Minutes.Waiting != 0 || s.Minutes.LateIgnored != 0 {
		t.Fatalf("status = %+v", s.Minutes)
	}
}

// A device without an input sensor (low signal): the editor sends
// heartbeats while the person reads (no write, same line), so those minutes
// are idle with the project. The agent stops at +1:50; at +2:10 the person
// saves a file, and wakatime-cli queues that heartbeat until the agent is
// back. Before ADR 0016 the restart at +47 froze +2 as idle before the
// queue flushed, and a replay with the flushed heartbeat credited it 60 s.
func TestOfflineHeartbeatAfterRestart(t *testing.T) {
	settle := func(o *crediting.Options) { o.Settle = 15 * time.Minute }
	reading := func(r *rig) {
		r.runEvery(m(-12), m(2), func(now time.Time) {
			prev := now.Truncate(time.Minute).Add(-time.Minute)
			r.heartbeat(prev.Add(10*time.Second), false)
			r.heartbeat(prev.Add(40*time.Second), false)
		})
	}

	// It arrives 46 minutes late, within the settle period after the
	// restart: it counts.
	t.Run("within settle", func(t *testing.T) {
		r := newRig(t, m(-12).Add(5*time.Second), settle)
		reading(r)
		r.restart()
		r.runEvery(m(47), m(48), nil)
		if got, rows := r.storedCredit(m(2)), credit(r.mustStored(m(2))); got != 0 || len(rows) != 1 {
			t.Fatalf("+2 before the flush = %v, want one idle row for app", rows)
		}
		r.runEvery(m(48), m(70), func(now time.Time) {
			if now.Truncate(time.Minute).Equal(m(48)) {
				r.pinned(m(48), func() { r.heartbeat(m(2).Add(10*time.Second), true) })
			}
		})
		if got := r.storedCredit(m(2)); got != 60 {
			t.Fatalf("+2 stored %d s, want 60 for the save that arrived within the settle period", got)
		}
		r.wantFinalMatchesReplay(m(-12), m(40))
		r.wantServerMatchesStored(m(2))
		if s := r.status(); s.Minutes.LateIgnored != 0 {
			t.Fatalf("late events ignored = %d, want 0", s.Minutes.LateIgnored)
		}
	})

	// It arrives at +75, after +2 was final: it is kept as evidence but
	// changes nothing, and status counts it.
	t.Run("after finality", func(t *testing.T) {
		r := newRig(t, m(-12).Add(5*time.Second), settle)
		reading(r)
		r.restart()
		r.runEvery(m(47), m(75), nil)
		before := credit(r.wantFinalMatchesReplay(m(-12), m(40)))
		r.pinned(m(75), func() { r.heartbeat(m(2).Add(10*time.Second), true) })
		r.runEvery(m(75), m(80), nil)
		if got := r.storedCredit(m(2)); got != 0 {
			t.Fatalf("+2 stored %d s after a heartbeat that arrived after it was final, want 0", got)
		}
		if after := credit(r.wantFinalMatchesReplay(m(-12), m(40))); !slices.Equal(after, before) {
			t.Fatalf("final minutes changed:\nbefore %v\nafter  %v", before, after)
		}
		r.wantServerMatchesStored(m(2))
		s := r.status()
		if s.Minutes.LateIgnored != 1 || !s.Minutes.LateLatest.Equal(m(75)) {
			t.Fatalf("late events = %d (latest %s), want 1 at +75", s.Minutes.LateIgnored, s.Minutes.LateLatest)
		}
		ev, err := r.db.EventsBetween(context.Background(), m(2), m(3), nil)
		if err != nil || len(ev.Heartbeats) != 1 || !ev.Heartbeats[0].Arrived.Equal(m(75)) {
			t.Fatalf("the late heartbeat is not kept with its arrival: %+v, %v", ev.Heartbeats, err)
		}
	})
}
