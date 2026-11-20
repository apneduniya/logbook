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

// MaxClockSkew is how far the system clock may differ from the server's
// before status says so. Days are sealed by the earlier of the two clocks
// whatever the difference.
const MaxClockSkew = 5 * time.Minute

// The system clock is trusted for minutes (the engine, the 30-minute
// finality) but two mistakes would lose credit for good:
//
//   - a clock set back into a day the evidence chain has already sealed
//     would compute minutes the server refuses as sealed. So the end of the
//     latest sealed (or skipped) day is kept (keySealedThrough), no minute
//     before it is computed or stored, and while the clock is before it
//     crediting pauses: no computing, uploading or sealing, until the
//     clock catches up;
//   - a clock set far ahead would seal days that haven't ended, and when it
//     came back the agent would pause until real time caught up. Event
//     times can't tell: events are stamped by the same clock. So a day is
//     sealed only once it has ended plus DayGrace by the system clock and
//     by the server's, read from the Date header of a minutes response in
//     the same run (an empty batch asks for it when the run sent none). A
//     server that sends no Date header leaves the system clock alone.

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
	cur, err := r.opt.Store.Get(ctx, keySealedThrough)
	if err != nil {
		return err
	}
	if day > cur {
		more = append(more, localdb.KV{Key: keySealedThrough, Value: day})
	}
	if len(more) == 0 {
		return nil
	}
	return r.opt.Store.SetMany(ctx, more...)
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

// observeServer records the server's clock from a response of this run and
// shows in status whether it differs much from the system clock.
func (r *Runner) observeServer(ctx context.Context, server time.Time) {
	if server.IsZero() {
		return
	}
	local := r.opt.Now()
	r.serverAt = server
	msg := ""
	switch skew := local.Sub(server); {
	case skew > MaxClockSkew:
		msg = "the system clock is ahead of the server's by more than 5 minutes; evidence days are sealed by the earlier of the two"
	case skew < -MaxClockSkew:
		msg = "the system clock is behind the server's by more than 5 minutes; evidence days are sealed by the earlier of the two"
	}
	if old := r.shown[keyClock]; old != msg && msg != "" {
		r.opt.Logger.WarnContext(ctx, "system clock differs from the server's", "local", local.UTC().Format(time.RFC3339),
			"server", server.UTC().Format(time.RFC3339))
	}
	r.show(ctx, keyClock, msg)
}

// serverClock returns the server's clock as seen in this run, asking for it
// with an empty minutes batch if the run has sent none. Zero means the
// server gave no Date header.
func (r *Runner) serverClock(ctx context.Context, t serverapi.Target, dev devicekey.Device) (time.Time, error) {
	if !r.serverAt.IsZero() {
		return r.serverAt, nil
	}
	res, err := r.opt.Server.UploadMinutes(ctx, t, dev.ID, serverapi.MinutesRequest{
		RulesVersion: struggle.RulesVersion, Minutes: []serverapi.Minute{},
	})
	if err != nil {
		return time.Time{}, err
	}
	r.observeServer(ctx, res.ServerTime)
	return res.ServerTime, nil
}
