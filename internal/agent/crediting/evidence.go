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
// kept for 90 days.
const retentionDays = 89

// sealAndUploadDays advances the device's evidence chain: it re-sends a day
// that was sealed but not yet accepted, then seals and uploads each ended
// day in order, up to maxDaysPerRun. A day's block seals the log rows that
// arrived since the previous block (ADR 0017), whatever their time; a day
// whose range holds no rows is skipped (a gap, which the server allows).
// The chain moves only when the server accepts a day; a conflict or
// rejection stops it for good and is shown in status, so a chain is never
// forked silently.
//
// The server refuses first-time minutes in a sealed day, so a day is sealed
// only when (ADR 0013):
//   - it has ended plus DayGrace by both the system clock and the server's,
//     and the two agree to within MaxClockSkew (sealingClock);
//   - every stored minute up to its end is final and acknowledged, so every
//     key of its minutes is at or below the block's end;
//   - this run's minute uploads went through (mayseal).
//
// Otherwise the day waits for a later run.
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

	prev := block{}
	day := utcDay(dev.RegisteredAt)
	if ok {
		prev, day = block{chainHash: last.ChainHash}, nextDay(last.Day)
		if !last.Legacy {
			// A legacy day covered rows by event time; the next block
			// starts again at the oldest retained row.
			prev.seqTo = last.SeqTo
		}
	}
	if cursor, err := r.cursor(ctx, dev.ID); err != nil {
		return err
	} else if cursor != "" && cursor >= day {
		day = nextDay(cursor)
	}
	if oldest := utcDay(now.AddDate(0, 0, -retentionDays)); day < oldest {
		day = oldest
	}
	if !mayseal || day > lastSealableDay(now) {
		return nil
	}
	// Seal (or skip) only days that have ended by the earlier of the system
	// clock and the server's, and only while the two agree: a clock far
	// ahead must never move the chain past a day that hasn't ended.
	at, agree, err := r.sealingClock(ctx, t, dev, now)
	if err != nil {
		return err
	}
	if !agree {
		r.opt.Logger.DebugContext(ctx, "not sealing evidence days: the server's clock is missing or disagrees", "next_day", day)
		return nil
	}
	end := lastSealableDay(at)
	for sealed := 0; day <= end && sealed < maxDaysPerRun; day = nextDay(day) {
		if waiting, err := r.minutesWaiting(ctx, day); err != nil || waiting > 0 {
			if waiting > 0 {
				r.opt.Logger.DebugContext(ctx, "evidence day waits for its minutes to be final and acknowledged", "day", day, "rows", waiting)
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
		prev = block{chainHash: cur.ChainHash, seqTo: cur.SeqTo}
	}
	return nil
}

// block is where the device's chain stands: the previous block's chain
// hash ("" before the first) and the last sequence number it sealed (0
// before the first, or after a legacy day).
type block struct {
	chainHash string
	seqTo     int64
}

// minutesWaiting counts stored rows up to the end of day that are open or
// not acknowledged. Earlier days count too: they must reach the server
// before a later day seals anything.
func (r *Runner) minutesWaiting(ctx context.Context, day string) (int64, error) {
	_, end, err := evidence.DayBounds(day)
	if err != nil {
		return 0, err
	}
	return r.opt.Store.MinutesPendingBefore(ctx, end)
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

// sealDay seals the log rows after prev as day's block, signs it and
// stores it as pending. The block's end, H_D, and its rows are read in one
// snapshot; rows in the range are never updated, and none can be deleted
// before they are sealed, so the stored range always holds exactly what was
// hashed. empty is true (and nothing is stored) when the range has no rows.
func (r *Runner) sealDay(ctx context.Context, dev devicekey.Device, day string, prev block) (localdb.EvidenceDay, bool, error) {
	var (
		rows       []evidence.Row
		from, high int64
	)
	err := r.opt.Store.View(ctx, func(s *localdb.Snapshot) error {
		high = s.H
		var err error
		if rows, err = s.Rows(ctx, prev.seqTo, s.H); err != nil {
			return err
		}
		from = prev.seqTo + 1
		if prev.seqTo == 0 {
			// A device's first block starts at the oldest retained row.
			from, err = s.OldestAbove(ctx, 0)
		}
		return err
	})
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("reading the log to seal %s: %w", day, err)
	}
	if len(rows) == 0 {
		return localdb.EvidenceDay{}, true, nil
	}
	content, err := evidence.ContentHash(rows)
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("hashing evidence for %s: %w", day, err)
	}
	d := evidence.Day{DeviceID: dev.ID, Day: day, PrevChainHash: prev.chainHash, ContentHash: content, RulesVersion: struggle.RulesVersion}
	chain, sig, err := dev.Key.Sign(d)
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("signing evidence for %s: %w", day, err)
	}
	e := localdb.EvidenceDay{DeviceID: dev.ID, Day: day, PrevChainHash: prev.chainHash, ContentHash: content,
		ChainHash: chain, Signature: sig, RulesVersion: d.RulesVersion, EventCounts: evidence.Counts(rows),
		SeqFrom: from, SeqTo: high, State: localdb.EvidencePending}
	if err := r.opt.Store.InsertEvidenceDay(ctx, e); err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	if err := r.markSealed(ctx, day); err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	r.opt.Logger.InfoContext(ctx, "sealed evidence day", "day", day, "rows", len(rows), "seq_from", from, "seq_to", high)
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
