package crediting

import (
	"context"
	"strconv"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
)

// Settling (ADR 0017, point 6) is about fairness only. While the agent runs,
// every event reaches it within seconds, so a minute is made final by its
// first run at minute + FinalLead. After the agent was down (stopped, or the
// machine asleep), events from that time still arrive: the hook spool is
// drained at start, wakatime-cli flushes its offline queue, Claude Code
// retries a delayed OTLP export. So after a start, a wake or a long gap in
// runs no minute becomes final until the agent has been up for
// Options.Settle and the startup spool drain has finished; meanwhile
// minutes are computed and stored, and those the server would freeze
// before the agent settles are not uploaded.
//
// None of this decides whether a replay agrees: every stored minute records
// the key it was computed at. If settling goes wrong, the worst case is an
// honest late event that isn't credited.

// settleState is whether this run may make minutes final.
type settleState struct {
	settled bool
	// at is when the agent settles: upSince + Settle.
	at time.Time
	// drained is set once the startup spool drain has finished.
	drained bool
}

// settle notes this run's time, starting a new settle period on the first
// run and after a wake or a gap in runs, and returns whether the agent has
// settled.
func (r *Runner) settle(ctx context.Context, now time.Time) settleState {
	// Round(0) drops the monotonic reading: the wall-clock gap. Sub of two
	// times that both have one uses the monotonic clock.
	wall, mono := now.Round(0).Sub(r.lastRun.Round(0)), now.Sub(r.lastRun)
	if r.upSince.IsZero() || woke(wall, mono, 2*r.opt.Interval+time.Minute) {
		if !r.upSince.IsZero() {
			r.opt.Logger.InfoContext(ctx, "no crediting run for a while (asleep or stopped); waiting for late events before making minutes final",
				"last_run", r.lastRun.UTC().Format(time.RFC3339))
		}
		r.upSince = now
	}
	r.lastRun = now
	st := settleState{at: r.upSince, drained: r.drained()}
	if r.opt.Settle > 0 {
		st.at = st.at.Add(r.opt.Settle)
	}
	st.settled = st.drained && !now.Round(0).Before(st.at.Round(0))
	r.show(ctx, keySettling, st.message())
	return st
}

// woke reports whether the time since the previous run, by the wall clock
// (wall) and the monotonic clock (mono), is a gap longer than gap by
// either. The wall clock sees a suspend, which the monotonic clock may not
// count; the monotonic clock sees a gap the wall clock hides when it is set
// back meanwhile. A wall clock set back past the previous run counts too.
func woke(wall, mono, gap time.Duration) bool {
	return wall > gap || wall < 0 || mono > gap
}

// drained reports whether the startup spool drain has finished.
func (r *Runner) drained() bool {
	if r.opt.Drained == nil {
		return true
	}
	select {
	case <-r.opt.Drained:
		return true
	default:
		return false
	}
}

// message is the status line while settling ("" once settled).
func (s settleState) message() string {
	switch {
	case s.settled:
		return ""
	case !s.drained:
		return "waiting for the hook spool to drain before making minutes final"
	}
	return "waiting until " + s.at.UTC().Format(time.RFC3339) +
		" for events that arrive late after a start or wake before making minutes final"
}

// holdBefore returns the open minutes that must not be uploaded yet: those
// before it. They are not final here, and the server would freeze them
// before the agent can make them final (it already holds as final every
// minute 30 minutes old). Uploading them now would make a provisional
// value final on the server. Zero once settled.
func holdBefore(now time.Time, st settleState) time.Time {
	if st.settled {
		return time.Time{}
	}
	at := st.at
	if !st.drained && now.After(at) {
		at = now // the drain may finish any time from now
	}
	open := ceilMinute(at.Add(-FinalLead))
	if fb := serverFinalBefore(now); open.Before(fb) {
		open = fb
	}
	return open
}

func ceilMinute(t time.Time) time.Time {
	m := t.UTC().Truncate(time.Minute)
	if m.Before(t) {
		m = m.Add(time.Minute)
	}
	return m
}

// lateRows counts, inside the run's snapshot and before the run stores
// anything, the raw events logged since the last run whose own minute is
// already final: they can never change its credit. They stay logged, and
// are sealed in the open evidence block like every row.
func (r *Runner) lateRows(ctx context.Context, snap *localdb.Snapshot) (localdb.LateResult, error) {
	if !r.lateKnown {
		mark, err := r.lateMark(ctx)
		if err != nil {
			return localdb.LateResult{}, err
		}
		if mark < 0 || mark > snap.H {
			// The first run since this was kept, or a replaced log: start
			// counting from now.
			mark = snap.H
		}
		r.late, r.lateKnown = mark, true
	}
	if r.late >= snap.H {
		return localdb.LateResult{}, nil
	}
	return snap.Late(ctx, r.late, snap.H)
}

// countLate records late events found by lateRows and moves the mark to h.
func (r *Runner) countLate(ctx context.Context, res localdb.LateResult, h int64) {
	if res.Count > 0 {
		r.opt.Logger.InfoContext(ctx, "events arrived after their minute was final; they don't change its credit",
			"events", res.Count, "latest", res.Latest.Format(time.RFC3339))
		if err := r.opt.Store.Add(ctx, keyLateIgnored, res.Count); err != nil {
			r.opt.Logger.WarnContext(ctx, "counting late events", "err", err)
		}
		r.persist(ctx, localdb.KV{Key: keyLateLatest, Value: formatTime(res.Latest)})
	}
	if h != r.late {
		r.late = h
		r.persist(ctx, localdb.KV{Key: keyLateMark, Value: strconv.FormatInt(h, 10)})
	}
}

// lateMark reads the persisted late event mark; -1 when unset or
// unreadable.
func (r *Runner) lateMark(ctx context.Context) (int64, error) {
	v, err := r.opt.Store.Get(ctx, keyLateMark)
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if v == "" || perr != nil {
		return -1, nil
	}
	return n, nil
}
