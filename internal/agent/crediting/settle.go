package crediting

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
)

// Settled finality (ADR 0016). A minute's final credit must not depend on
// when the agent happened to compute it. While the agent runs, every event
// reaches it within seconds, so a minute is made final by the run at
// minute + FinalLead, with that run's arrival cutoff. After the agent was
// down (stopped, or the machine asleep), events from that time still
// arrive: the hook spool is drained at start, wakatime-cli flushes its
// offline queue. So after a start or a gap in runs no minute becomes final
// until the agent has been up for Options.Settle and the startup spool
// drain has finished; meanwhile minutes are computed for the status page
// and stored, but those the server would freeze before the agent settles
// are not uploaded.

// settleState is whether this run may make minutes final.
type settleState struct {
	settled bool
	// at is when the agent settles: upSince + Settle.
	at time.Time
	// drained is set once the startup spool drain has finished.
	drained bool
}

// settle notes this run's time, starting a new settle period on the first
// run and after a gap in runs, and returns whether the agent has settled.
func (r *Runner) settle(ctx context.Context, now time.Time) settleState {
	gap := 2*r.opt.Interval + time.Minute
	// A clock set back before the settle period began starts it again too.
	if r.upSince.IsZero() || now.Sub(r.lastRun) > gap || now.Before(r.upSince) {
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
	st.settled = st.drained && !now.Before(st.at)
	r.show(ctx, keySettling, st.message())
	return st
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
		" for events that arrive late after a start before making minutes final"
}

// held returns the minutes that must not be uploaded yet: not final here,
// and old enough that the server would freeze them before the agent can
// make them final. Uploading them now would make their provisional value
// final on the server.
func (r *Runner) held(ctx context.Context, now time.Time, st settleState) (localdb.Span, error) {
	if st.settled {
		return localdb.Span{}, nil
	}
	ft, err := r.opt.Store.FinalThrough(ctx)
	if err != nil {
		return localdb.Span{}, err
	}
	at := st.at
	if !st.drained && now.After(at) {
		at = now // the drain may finish any time from now
	}
	open := ceilMinute(at.Add(-FinalLead))
	if fb := finalBefore(now); open.Before(fb) {
		open = fb
	}
	return localdb.Span{From: ft, To: open}, nil
}

func ceilMinute(t time.Time) time.Time {
	m := t.UTC().Truncate(time.Minute)
	if m.Before(t) {
		m = m.Add(time.Minute)
	}
	return m
}

// countLate counts the raw events that arrived by cutoff for minutes that
// were already final before this run (before wasFinal): they never change
// those minutes' credit. They stay stored, and in their day's evidence.
func (r *Runner) countLate(ctx context.Context, cutoff, wasFinal time.Time) {
	if !r.lateKnown {
		mark, ok, err := r.lateMark(ctx)
		if err != nil {
			r.opt.Logger.WarnContext(ctx, "reading the late event mark", "err", err)
			return
		}
		r.late, r.lateKnown = mark, true
		if !ok {
			// The first run since this was added: start from now.
			if r.late, err = r.opt.Store.HighWater(ctx); err != nil {
				r.lateKnown = false
				return
			}
			r.persist(ctx, localdb.KV{Key: keyLateMark, Value: formatMark(r.late)})
			return
		}
	}
	res, next, err := r.opt.Store.LateSince(ctx, r.late, cutoff, wasFinal)
	if err != nil {
		if ctx.Err() == nil {
			r.opt.Logger.WarnContext(ctx, "counting late events", "err", err)
		}
		return
	}
	if res.Count > 0 {
		r.opt.Logger.InfoContext(ctx, "events arrived after their minute was final; they don't change its credit",
			"events", res.Count, "latest", res.Latest.Format(time.RFC3339))
		if err := r.opt.Store.Add(ctx, keyLateIgnored, res.Count); err != nil {
			r.opt.Logger.WarnContext(ctx, "counting late events", "err", err)
		}
		r.persist(ctx, localdb.KV{Key: keyLateLatest, Value: formatTime(res.Latest)})
	}
	if next != r.late {
		r.late = next
		r.persist(ctx, localdb.KV{Key: keyLateMark, Value: formatMark(next)})
	}
}

// lateMark reads the persisted late event mark; ok is false when unset or
// unreadable.
func (r *Runner) lateMark(ctx context.Context) (localdb.HighWater, bool, error) {
	v, err := r.opt.Store.Get(ctx, keyLateMark)
	if err != nil || v == "" {
		return localdb.HighWater{}, false, err
	}
	var hw localdb.HighWater
	if _, err := fmt.Sscan(v, &hw.Heartbeats, &hw.AI, &hw.Activity, &hw.Git); err != nil {
		return localdb.HighWater{}, false, nil //nolint:nilerr // a damaged mark restarts counting from now
	}
	return hw, true, nil
}

func formatMark(hw localdb.HighWater) string {
	return strconv.FormatInt(hw.Heartbeats, 10) + " " + strconv.FormatInt(hw.AI, 10) + " " +
		strconv.FormatInt(hw.Activity, 10) + " " + strconv.FormatInt(hw.Git, 10)
}
