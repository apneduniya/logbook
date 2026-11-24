package crediting

import (
	"context"
	"fmt"
	"time"

	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// MaxClockSkew is how far the system clock may differ from the server's.
// Beyond it no day is sealed (or skipped as empty), and status says so.
const MaxClockSkew = 5 * time.Minute

// The system clock is trusted for minutes (the engine, the 30-minute
// finality) but two mistakes would lose credit for good:
//
//   - a clock set back into a day the evidence chain has already sealed
//     would compute minutes the server refuses as sealed. So the end of the
//     latest sealed (or skipped) day is kept (keySealedThrough), no minute
//     before it is computed or stored, and while the clock is before it
//     crediting pauses: no computing, uploading or sealing, until the
//     clock catches up. A day counts as sealed from the moment its block's
//     bound is frozen (reserveDay), before it is hashed: a minute computed
//     later would have a key past the bound, which its block can't replay;
//   - a clock set far ahead would seal days that haven't ended (or skip
//     them as empty), and when it came back the agent would pause until
//     real time caught up. Event times can't tell: events are stamped by
//     the same clock. So the chain moves only while the system clock and
//     the server's agree to within MaxClockSkew, and only over days that
//     have ended plus DayGrace by the earlier of the two. The server's
//     clock is the server_time in the body of a minutes response in the
//     same run (an empty batch asks for it when the run sent none), never
//     a Date header, which a proxy may drop or rewrite. If the server's
//     time is missing, malformed or disagrees, nothing is sealed; minutes
//     are still computed and uploaded, and sealing catches up in order
//     once the clocks agree.

// sealedThrough returns the end of the latest UTC day the evidence chain
// has moved past (sealed, pending or found empty), for any device, or zero.
// Older agents kept only the evidence keys, so those count too.
func (r *Runner) sealedThrough(ctx context.Context) (time.Time, error) {
	latest := ""
	for _, key := range []string{keySealedThrough, keyEvidenceDay, keyEvidenceCursor} {
		v, err := r.opt.Store.Get(ctx, key)
		if err != nil {
			return time.Time{}, err
		}
		if key == keyEvidenceCursor && len(v) > len(time.DateOnly) {
			v = v[len(v)-len(time.DateOnly):] // "<device id> <day>"
		}
		if v > latest {
			latest = v
		}
	}
	if latest == "" {
		return time.Time{}, nil
	}
	_, end, err := evidence.DayBounds(latest)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the last sealed day %q: %w", latest, err)
	}
	return end, nil
}

// markSealed records that the chain has moved past day.
func (r *Runner) markSealed(ctx context.Context, day string, more ...localdb.KV) error {
	marks, err := r.sealedMarks(ctx, day)
	if err != nil {
		return err
	}
	if more = append(more, marks...); len(more) == 0 {
		return nil
	}
	return r.opt.Store.SetMany(ctx, more...)
}

// sealedMarks returns the mark that moves keySealedThrough to day, none if
// it is there or past it already: it only grows.
func (r *Runner) sealedMarks(ctx context.Context, day string) ([]localdb.KV, error) {
	cur, err := r.opt.Store.Get(ctx, keySealedThrough)
	if err != nil {
		return nil, err
	}
	if day > cur {
		return []localdb.KV{{Key: keySealedThrough, Value: day}}, nil
	}
	return nil, nil
}

// checkClock reports whether crediting may run at now: not while now is
// before through, the end of the last sealed day. Entering and leaving that
// state is logged once each and shown in status.
func (r *Runner) checkClock(ctx context.Context, now, through time.Time) bool {
	if now.Before(through) {
		msg := fmt.Sprintf("the system clock is behind the last sealed day, which ended %s; "+
			"crediting is paused until the clock passes it. Check the date and time settings.",
			through.UTC().Format(time.RFC3339))
		if !r.clockBehind {
			r.clockBehind = true
			r.opt.Logger.WarnContext(ctx, "system clock is behind the last sealed day; crediting paused",
				"now", now.UTC().Format(time.RFC3339), "sealed_through", through.UTC().Format(time.RFC3339))
		}
		r.show(ctx, keyClock, msg)
		return false
	}
	if r.clockBehind {
		r.opt.Logger.InfoContext(ctx, "system clock caught up with the last sealed day; crediting resumed")
	}
	if r.clockBehind || !r.clockChecked {
		// Clear a pause message, also one left by an earlier process; a
		// difference with the server is shown again when next measured.
		r.show(ctx, keyClock, "")
	}
	r.clockBehind, r.clockChecked = false, true
	return true
}

// serverClock is what one run learned of the server's clock.
type serverClock struct {
	// seen is set once a minutes response arrived in this run.
	seen bool
	// at is that response's server_time (zero if missing or malformed),
	// and local the system clock when it arrived.
	at, local time.Time
}

// agrees reports whether the server gave its time and it is within
// MaxClockSkew of the system clock.
func (c serverClock) agrees() bool {
	skew := c.local.Sub(c.at)
	return !c.at.IsZero() && skew <= MaxClockSkew && skew >= -MaxClockSkew
}

// message is the status line for c ("" when the clocks agree).
func (c serverClock) message() string {
	switch skew := c.local.Sub(c.at); {
	case c.at.IsZero():
		return "the server did not report its time, so evidence days are not sealed; " +
			"credited minutes are still uploaded. Updating the server may help."
	case skew > MaxClockSkew:
		return "the system clock is ahead of the server's by more than 5 minutes, so evidence days are not sealed " +
			"until they agree; credited minutes are still uploaded. Check the date and time settings."
	case skew < -MaxClockSkew:
		return "the system clock is behind the server's by more than 5 minutes, so evidence days are not sealed " +
			"until they agree; credited minutes are still uploaded. Check the date and time settings."
	}
	return ""
}

// observeServer records the server's clock from a minutes response of this
// run, notes how far the system clock is ahead of it (for finality), and
// shows in status whether it can be trusted for sealing.
func (r *Runner) observeServer(ctx context.Context, server time.Time) {
	r.server = serverClock{seen: true, at: server, local: r.opt.Now()}
	r.ahead, r.aheadKnown = 0, !server.IsZero()
	if r.aheadKnown {
		r.ahead = finalityOffset(r.server.local.Sub(server))
	}
	msg := r.server.message()
	if old := r.shown[keyClock]; old != msg && msg != "" {
		r.opt.Logger.WarnContext(ctx, "the server's clock is missing or differs from the system clock; not sealing evidence days",
			"local", r.server.local.UTC().Format(time.RFC3339), "server", server.UTC().Format(time.RFC3339))
	}
	r.show(ctx, keyClock, msg)
}

// aheadSlack is the clock offset finality ignores. Every reading looks a
// little ahead (the server stamps server_time when it starts handling the
// request, the agent reads its clock when the answer arrives), and
// subtracting that would push each minute's finality a whole minute later,
// past the server's 30-minute freeze. Finalizing up to aheadSlack early
// still leaves FinalLead's minute of margin before the server freezes.
const aheadSlack = 30 * time.Second

// finalityOffset is how far finality runs behind the system clock, given
// how far the system clock reads ahead of the server's (negative when
// behind).
func finalityOffset(ahead time.Duration) time.Duration {
	if ahead <= aheadSlack {
		return 0
	}
	return ahead
}

// sealingClock returns the clock days are sealed by: the earlier of now and
// the server's clock, read in this run (an empty minutes batch asks for it
// if the run has sent none). ok is false, and nothing may be sealed or
// skipped, when the server gave no usable time or the two clocks disagree
// by more than MaxClockSkew.
func (r *Runner) sealingClock(ctx context.Context, t serverapi.Target, dev devicekey.Device, now time.Time) (at time.Time, ok bool, err error) {
	if err := r.askServerTime(ctx, t, dev); err != nil {
		return time.Time{}, false, err
	}
	if !r.server.agrees() {
		return time.Time{}, false, nil
	}
	if r.server.at.Before(now) {
		return r.server.at, true, nil
	}
	return now, true, nil
}

// askServerTime sends an empty minutes batch, which only asks for the
// server's clock, unless a minutes response in this run already gave it.
func (r *Runner) askServerTime(ctx context.Context, t serverapi.Target, dev devicekey.Device) error {
	if r.server.seen {
		return nil
	}
	res, err := r.opt.Server.UploadMinutes(ctx, t, dev.ID, serverapi.MinutesRequest{
		RulesVersion: struggle.RulesVersion, Minutes: []serverapi.Minute{},
	})
	if err != nil {
		return err
	}
	r.observeServer(ctx, res.ServerTime)
	return nil
}
