package crediting

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// retentionDays is how far back a day can still be sealed: raw events are
// kept for 90 days, so older days can't be reproduced.
const retentionDays = 89

// sealAndUploadDays advances the device's evidence chain: it re-sends a day
// that was sealed but not yet accepted, then seals and uploads each ended
// day in order, up to maxDaysPerRun. Days without any events are skipped (a
// gap, which the server allows). The chain moves only when the server
// accepts a day; a conflict or rejection stops it for good and is shown in
// status, so a chain is never forked silently.
//
// The server refuses first-time minutes in a sealed day, so a new day is
// sealed only when mayseal (this run's minute uploads went through, not on
// hold or refused) and the server has answered for every stored minute of
// it. Otherwise the day waits for a later run. The engine never writes
// minutes into a sealable day again: its window is the last hour, and a day
// becomes sealable DayGrace (an hour) after it ends.
func (r *Runner) sealAndUploadDays(ctx context.Context, t serverapi.Target, dev devicekey.Device, now time.Time, mayseal bool) error {
	last, ok, err := r.opt.Store.LastEvidenceDay(ctx, dev.ID)
	if err != nil {
		return err
	}
	if ok && last.State == localdb.EvidencePending {
		if err := r.uploadDay(ctx, t, last); err != nil {
			return err
		}
		if last, _, err = r.opt.Store.LastEvidenceDay(ctx, dev.ID); err != nil {
			return err
		}
	}
	if ok && (last.State == localdb.EvidenceConflict || last.State == localdb.EvidenceRejected) {
		return nil
	}

	prev := ""
	day := utcDay(dev.RegisteredAt)
	if ok {
		prev, day = last.ChainHash, nextDay(last.Day)
	}
	if cursor, err := r.cursor(ctx, dev.ID); err != nil {
		return err
	} else if cursor != "" && cursor >= day {
		day = nextDay(cursor)
	}
	if oldest := utcDay(now.AddDate(0, 0, -retentionDays)); day < oldest {
		day = oldest
	}
	end := lastSealableDay(now)
	if mayseal && day <= end {
		// Seal by the earlier of the system clock and the server's, so a
		// clock far ahead never seals a day that hasn't ended.
		server, err := r.serverClock(ctx, t, dev)
		if err != nil {
			return err
		}
		if !server.IsZero() && lastSealableDay(server) < end {
			end = lastSealableDay(server)
		}
	}
	for sealed := 0; mayseal && day <= end && sealed < maxDaysPerRun; day = nextDay(day) {
		if waiting, err := r.minutesWaiting(ctx, day); err != nil || waiting > 0 {
			if waiting > 0 {
				r.opt.Logger.DebugContext(ctx, "evidence day waits for its minutes", "day", day, "minutes", waiting)
			}
			return err
		}
		e, empty, err := r.sealDay(ctx, dev, day, prev)
		if err != nil {
			return err
		}
		if empty {
			if err := r.markSealed(ctx, day, localdb.KV{Key: keyEvidenceCursor, Value: dev.ID + " " + day}); err != nil {
				return err
			}
			continue
		}
		sealed++
		if err := r.uploadDay(ctx, t, e); err != nil {
			return err
		}
		cur, _, err := r.opt.Store.LastEvidenceDay(ctx, dev.ID)
		if err != nil {
			return err
		}
		if cur.State != localdb.EvidenceAccepted {
			return nil // conflict or rejected: the chain stops here
		}
		prev = cur.ChainHash
	}
	return nil
}

// minutesWaiting counts stored minutes up to the end of day the server
// hasn't answered for. Earlier days count too: they must reach the server
// before a later day seals anything.
func (r *Runner) minutesWaiting(ctx context.Context, day string) (int64, error) {
	_, end, err := evidence.DayBounds(day)
	if err != nil {
		return 0, err
	}
	return r.opt.Store.UnsentMinutesBefore(ctx, end)
}

// cursor returns the last day scanned for dev, or "".
func (r *Runner) cursor(ctx context.Context, deviceID string) (string, error) {
	v, err := r.opt.Store.Get(ctx, keyEvidenceCursor)
	if err != nil {
		return "", err
	}
	dev, day, ok := strings.Cut(v, " ")
	if !ok || dev != deviceID {
		return "", nil
	}
	return day, nil
}

// sealDay hashes and signs one day's events and stores it as pending.
// empty is true (and nothing is stored) when the day has no events.
func (r *Runner) sealDay(ctx context.Context, dev devicekey.Device, day, prev string) (localdb.EvidenceDay, bool, error) {
	start, end, err := evidence.DayBounds(day)
	if err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	hw, err := r.opt.Store.HighWater(ctx)
	if err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	var active []struggle.ActiveTime
	if r.opt.ActiveTimes != nil {
		// Datapoints belong to the day their To is in; read a little either
		// side in case the source filters by From.
		if active, err = r.opt.ActiveTimes.ActiveTimes(ctx, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
			return localdb.EvidenceDay{}, false, fmt.Errorf("reading active time for %s: %w", day, err)
		}
	}
	// What is hashed is exactly what ReplayDay rebuilds from the stored day:
	// raw event rows up to hw, and these active-time records, kept as is.
	e := localdb.EvidenceDay{DeviceID: dev.ID, Day: day, PrevChainHash: prev, HighWater: hw,
		ActiveTime: evidenceActiveTimes(active, start, end), RulesVersion: struggle.RulesVersion,
		State: localdb.EvidencePending}
	events, err := dayEvents(ctx, r.opt.Store, e)
	if err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	counts := evidence.Counts(events)
	total := 0
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		return localdb.EvidenceDay{}, true, nil
	}
	content, err := evidence.ContentHash(events)
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("hashing evidence for %s: %w", day, err)
	}
	d := evidence.Day{DeviceID: dev.ID, Day: day, PrevChainHash: prev, ContentHash: content, RulesVersion: e.RulesVersion}
	chain, sig, err := dev.Key.Sign(d)
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("signing evidence for %s: %w", day, err)
	}
	e.ContentHash, e.ChainHash, e.Signature, e.EventCounts = content, chain, sig, counts
	if err := r.opt.Store.InsertEvidenceDay(ctx, e); err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	if err := r.markSealed(ctx, day); err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	r.opt.Logger.InfoContext(ctx, "sealed evidence day", "day", day, "events", total)
	return e, false, nil
}

// EventReader reads raw local events. *localdb.DB satisfies it.
type EventReader interface {
	EventsBetween(ctx context.Context, from, to time.Time, upto *localdb.HighWater) (localdb.Events, error)
}

// ErrNotReproducible is returned by ReplayDay when a sealed day's events
// can no longer be rebuilt locally (pruned, trimmed, or sealed before the
// agent kept active-time records).
var ErrNotReproducible = errors.New("sealed day can no longer be rebuilt from local data")

// ReplayDay rebuilds the events a sealed day's content hash covers: every
// raw event row of the day up to the day's high-water ids, and the
// active-time records kept with it. Events that arrived after sealing, and
// later changes to derived values, don't change the result, so hashing it
// gives the sealed content hash (ADR 0013). It returns ErrNotReproducible
// when local data no longer holds the day as sealed.
func ReplayDay(ctx context.Context, events EventReader, e localdb.EvidenceDay) (evidence.DayEvents, error) {
	if e.ActiveTime == nil && e.EventCounts[evidence.TypeActiveTime] > 0 {
		return evidence.DayEvents{}, fmt.Errorf("%s: active time not kept: %w", e.Day, ErrNotReproducible)
	}
	out, err := dayEvents(ctx, events, e)
	if err != nil {
		return evidence.DayEvents{}, err
	}
	if got := evidence.Counts(out); !maps.Equal(got, e.EventCounts) {
		return evidence.DayEvents{}, fmt.Errorf("%s: event counts %v, sealed %v: %w", e.Day, got, e.EventCounts, ErrNotReproducible)
	}
	return out, nil
}

// dayEvents is the day's content: raw rows up to e.HighWater plus
// e.ActiveTime.
func dayEvents(ctx context.Context, events EventReader, e localdb.EvidenceDay) (evidence.DayEvents, error) {
	start, end, err := evidence.DayBounds(e.Day)
	if err != nil {
		return evidence.DayEvents{}, err
	}
	hw := e.HighWater
	ev, err := events.EventsBetween(ctx, start, end, &hw)
	if err != nil {
		return evidence.DayEvents{}, err
	}
	return evidence.DayEvents{Day: e.Day, Activity: ev.Activity, Heartbeats: ev.Heartbeats, AI: ev.AI, Git: ev.Git,
		ActiveTime: e.ActiveTime}, nil
}

// uploadDay sends a sealed day and records the answer. Conflicts and
// rejections are recorded (and stop the chain); other failures are returned
// for backoff, leaving the day pending.
func (r *Runner) uploadDay(ctx context.Context, t serverapi.Target, e localdb.EvidenceDay) error {
	err := r.opt.Server.UploadEvidenceDay(ctx, t, e.DeviceID, serverapi.EvidenceDayRequest{
		Day: e.Day, PrevChainHash: e.PrevChainHash, ContentHash: e.ContentHash, ChainHash: e.ChainHash,
		Signature: e.Signature, RulesVersion: e.RulesVersion, EventCounts: e.EventCounts,
	})
	if err == nil {
		r.opt.Logger.InfoContext(ctx, "evidence day accepted", "day", e.Day)
		return r.opt.Store.SetEvidenceState(ctx, e.DeviceID, e.Day, localdb.EvidenceAccepted, "",
			localdb.KV{Key: keyEvidenceDevice, Value: e.DeviceID}, localdb.KV{Key: keyEvidenceDay, Value: e.Day},
			localdb.KV{Key: keyEvidenceHash, Value: e.ChainHash}, localdb.KV{Key: keyEvidenceState, Value: ""},
			localdb.KV{Key: keyEvidenceError, Value: ""},
			localdb.KV{Key: keyEvidenceCursor, Value: e.DeviceID + " " + e.Day})
	}
	var state string
	switch serverapi.KindOf(err) {
	case serverapi.KindConflict:
		state = localdb.EvidenceConflict
	case serverapi.KindInvalid:
		state = localdb.EvidenceRejected
	default:
		return err
	}
	msg := fmt.Sprintf("the server refused evidence for %s (%v); the evidence chain is stopped", e.Day, err)
	r.opt.Logger.ErrorContext(ctx, "evidence day refused; not advancing the chain", "day", e.Day, "state", state, "err", err)
	return r.opt.Store.SetEvidenceState(ctx, e.DeviceID, e.Day, state, err.Error(),
		localdb.KV{Key: keyEvidenceDevice, Value: e.DeviceID},
		localdb.KV{Key: keyEvidenceState, Value: state}, localdb.KV{Key: keyEvidenceError, Value: msg})
}
