package crediting

import (
	"context"
	"fmt"
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
func (r *Runner) sealAndUploadDays(ctx context.Context, t serverapi.Target, dev devicekey.Device, now time.Time) error {
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
	for sealed := 0; day <= end && sealed < maxDaysPerRun; day = nextDay(day) {
		e, empty, err := r.sealDay(ctx, dev, day, prev)
		if err != nil {
			return err
		}
		if empty {
			r.persist(ctx, localdb.KV{Key: keyEvidenceCursor, Value: dev.ID + " " + day})
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
	ev, err := r.opt.Store.EventsBetween(ctx, start, end, &hw)
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
	events := evidence.DayEvents{Day: day, Activity: ev.Activity, Heartbeats: ev.Heartbeats, AI: ev.AI, Git: ev.Git,
		ActiveTime: evidenceActiveTimes(active, start, end)}
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
	d := evidence.Day{DeviceID: dev.ID, Day: day, PrevChainHash: prev, ContentHash: content, RulesVersion: struggle.RulesVersion}
	chain, sig, err := dev.Key.Sign(d)
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("signing evidence for %s: %w", day, err)
	}
	e := localdb.EvidenceDay{
		DeviceID: dev.ID, Day: day, PrevChainHash: prev, ContentHash: content, ChainHash: chain, Signature: sig,
		RulesVersion: d.RulesVersion, EventCounts: counts, HighWater: hw, State: localdb.EvidencePending,
	}
	if err := r.opt.Store.InsertEvidenceDay(ctx, e); err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	r.opt.Logger.InfoContext(ctx, "sealed evidence day", "day", day, "events", total)
	return e, false, nil
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
