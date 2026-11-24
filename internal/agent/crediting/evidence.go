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

const (
	// retentionDays is how far back a day can still be sealed: raw events
	// are kept for 90 days.
	retentionDays = 89
	// DefaultSealRows is how many rows one run hashes at most (Options.
	// SealRows). A busy day is about 30 000 rows; a device's first block
	// can hold 90 days of them and is hashed over several runs.
	DefaultSealRows = 200_000
	// sealSpan is how many minutes of a block one hashing step reads.
	sealSpan = time.Hour
)

// sealAndUploadDays advances the device's evidence chain: it re-sends a day
// that was sealed but not yet accepted, finishes hashing a block whose
// bound is already frozen, then seals and uploads each ended day in order,
// up to maxDaysPerRun. A day's block seals the log rows logged since the
// previous block (ADR 0017, ADR 0019), whatever their time; a day whose
// range holds no row but repo rows is skipped (a gap, which the server
// allows). The chain moves only when the server accepts a day; a conflict
// or rejection stops it for good and is shown in status, so a chain is
// never forked silently.
//
// The server refuses first-time minutes in a sealed day, so a day is sealed
// only when (ADR 0013):
//   - it has ended plus DayGrace by both the system clock and the server's,
//     and the two agree to within MaxClockSkew (sealingClock);
//   - every stored minute up to its end is final and acknowledged, so every
//     key of its minutes is at or below the block's end;
//   - this run's minute uploads went through (mayseal).
//
// Otherwise the day waits for a later run. Once they hold, sealing a day
// takes three steps (ADR 0019 point 4): reserveDay freezes the block's
// bound and closes the day to minutes, hashBlock hashes the block over as
// many runs as it needs, and uploadDay sends it.
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
	budget := r.opt.SealRows
	// A block whose bound is frozen is finished first, whatever the clocks
	// say now: its day is already closed to minutes.
	s, sealing, err := r.opt.Store.Sealing(ctx)
	if err != nil {
		return err
	}
	if sealing {
		if s.DeviceID != dev.ID {
			// StartChain drops another device's block, so this is a bug.
			return fmt.Errorf("the block being sealed (%s) belongs to device %s, not %s", s.Day, s.DeviceID, dev.ID)
		}
		e, done, err := r.hashBlock(ctx, dev, s, &budget)
		if err != nil || !done {
			return err
		}
		if accepted, err := r.uploadSealed(ctx, t, e); err != nil || !accepted {
			return err
		}
		if last, ok, err = r.opt.Store.LastEvidenceDay(ctx, dev.ID); err != nil {
			return err
		}
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
	} else if prev.seqTo, err = r.opt.Store.ChainBaseline(ctx, dev.ID); err != nil {
		return err
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
		s, ok, err := r.reserveDay(ctx, dev, day, prev)
		if err != nil {
			return err
		}
		if !ok {
			if err := r.markSealed(ctx, day, localdb.KV{Key: keyEvidenceCursor, Value: dev.ID + " " + day}); err != nil {
				return err
			}
			continue
		}
		sealed++
		e, done, err := r.hashBlock(ctx, dev, s, &budget)
		if err != nil || !done {
			return err
		}
		accepted, err := r.uploadSealed(ctx, t, e)
		if err != nil || !accepted {
			return err
		}
		prev = block{chainHash: e.ChainHash, seqTo: e.SeqTo}
	}
	return nil
}

// block is where the device's chain stands: the previous block's chain
// hash ("" before the first) and the last sequence number it sealed (the
// device's baseline before its first block, 0 after a legacy day).
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

// reserveDay freezes day's block (ADR 0019 point 4): in one transaction
// the log's high-water mark becomes its upper bound, a repo snapshot opens
// the next range, and the day is marked sealed (keySealedThrough), so from
// then on no run computes or stores a minute of it or earlier, even after
// a restart or with the clock set back. Every minute of the day is final
// and acknowledged by then, so every key of its minutes is at or below the
// bound. ok is false when the range holds no row but repo rows: nothing is
// stored, and the day is a gap.
func (r *Runner) reserveDay(ctx context.Context, dev devicekey.Device, day string, prev block) (localdb.Sealing, bool, error) {
	marks, err := r.sealedMarks(ctx, day)
	if err != nil {
		return localdb.Sealing{}, false, err
	}
	marks = append(marks, localdb.KV{Key: keyEvidenceSealing, Value: day})
	s, ok, err := r.opt.Store.ReserveSeal(ctx, dev.ID, day, prev.chainHash, prev.seqTo, marks...)
	if err != nil {
		return localdb.Sealing{}, false, fmt.Errorf("reserving %s for sealing: %w", day, err)
	}
	if ok {
		r.opt.Logger.InfoContext(ctx, "sealing evidence day", "day", day, "seq_from", s.SeqFrom, "seq_to", s.SeqTo)
	}
	return s, ok, nil
}

// hashBlock hashes the block being sealed from where it stopped: first the
// repo bucket, then the minute buckets, a step of sealSpan at a time, in
// (bucket key, seq) order, until it has read budget rows (shared by the
// run) or the end of the block. Nothing can join the range any more, so
// the cursor can't miss a row. Each run's progress (the buckets closed and
// the hasher's state) is saved in one transaction, so a restart resumes
// from it. At the end it checks the bucket list against the root, signs
// the block, and stores it as pending with its bucket list; done is true
// and the stored day is returned.
func (r *Runner) hashBlock(ctx context.Context, dev devicekey.Device, s localdb.Sealing, budget *int) (localdb.EvidenceDay, bool, error) {
	h := evidence.NewHasher(r.opt.Salts)
	if s.Hasher != nil {
		if err := h.UnmarshalBinary(s.Hasher); err != nil {
			return localdb.EvidenceDay{}, false, fmt.Errorf("resuming the hash of %s: %w", s.Day, err)
		}
	}
	var (
		cursor   = s.Cursor
		closed   []evidence.Bucket
		stored   []evidence.Bucket
		finished bool
	)
	add := func(rows []evidence.Row) error {
		sorted, err := evidence.SortForHashing(rows)
		if err != nil {
			return err
		}
		c, err := h.Add(sorted)
		closed = append(closed, c...)
		*budget -= len(rows)
		return err
	}
	err := r.opt.Store.View(ctx, func(snap *localdb.Snapshot) error {
		if cursor == evidence.RepoKey {
			rows, err := snap.RepoRows(ctx, s.SeqFrom, s.SeqTo)
			if err != nil {
				return err
			}
			if err := add(rows); err != nil {
				return err
			}
			cursor = evidence.RepoKey + 1
		}
		for *budget > 0 {
			next, ok, err := snap.NextMinuteRow(ctx, s.SeqFrom, s.SeqTo, evidence.KeyMinute(cursor))
			if err != nil {
				return err
			}
			if !ok {
				finished = true
				stored, err = snap.Buckets(ctx, s.DeviceID, s.Day)
				return err
			}
			end := next.Add(sealSpan)
			rows, err := snap.MinuteRows(ctx, s.SeqFrom, s.SeqTo, next, end)
			if err != nil {
				return err
			}
			if err := add(rows); err != nil {
				return err
			}
			cursor = evidence.MinuteKey(end)
		}
		return nil
	})
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("hashing evidence for %s: %w", s.Day, err)
	}
	if !finished {
		state, err := h.MarshalBinary()
		if err != nil {
			return localdb.EvidenceDay{}, false, err
		}
		if err := r.opt.Store.SaveSealStep(ctx, s, cursor, state, closed); err != nil {
			return localdb.EvidenceDay{}, false, fmt.Errorf("saving the hash of %s: %w", s.Day, err)
		}
		r.opt.Logger.InfoContext(ctx, "evidence day partly hashed; continuing next run", "day", s.Day,
			"next_minute", evidence.KeyMinute(cursor).Format(time.RFC3339))
		return localdb.EvidenceDay{}, false, nil
	}
	blk, rest, err := h.Finish()
	if err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	closed = append(closed, rest...)
	root, err := evidence.Root(append(stored, closed...))
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("checking the bucket list of %s: %w", s.Day, err)
	}
	if root.String() != blk.ContentHash {
		return localdb.EvidenceDay{}, false, fmt.Errorf("the bucket list of %s hashes to %s, not %s", s.Day, root, blk.ContentHash)
	}
	d := evidence.Day{DeviceID: dev.ID, Day: s.Day, PrevChainHash: s.PrevChainHash, ContentHash: blk.ContentHash,
		RulesVersion: struggle.RulesVersion, SeqFrom: s.SeqFrom, SeqTo: s.SeqTo, Buckets: blk.Buckets,
		TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo}
	chain, sig, err := dev.Key.Sign(d)
	if err != nil {
		return localdb.EvidenceDay{}, false, fmt.Errorf("signing evidence for %s: %w", s.Day, err)
	}
	e := localdb.EvidenceDay{DeviceID: dev.ID, Day: s.Day, PrevChainHash: s.PrevChainHash, ContentHash: d.ContentHash,
		ChainHash: chain, Signature: sig, RulesVersion: d.RulesVersion, EventCounts: blk.Counts,
		SeqFrom: s.SeqFrom + 1, SeqTo: s.SeqTo, Buckets: blk.Buckets, TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo,
		State: localdb.EvidencePending}
	if err := r.opt.Store.CompleteSeal(ctx, s, closed, e, localdb.KV{Key: keyEvidenceSealing, Value: ""}); err != nil {
		return localdb.EvidenceDay{}, false, err
	}
	r.opt.Logger.InfoContext(ctx, "sealed evidence day", "day", s.Day, "rows", blk.Rows, "buckets", blk.Buckets,
		"seq_from", s.SeqFrom, "seq_to", s.SeqTo)
	return e, true, nil
}

// uploadSealed sends a newly sealed day and reports whether the server
// accepted it (a conflict or rejection stops the chain).
func (r *Runner) uploadSealed(ctx context.Context, t serverapi.Target, e localdb.EvidenceDay) (bool, error) {
	if err := r.uploadDay(ctx, t, e); err != nil {
		return false, err
	}
	cur, _, err := r.opt.Store.LastEvidenceDay(ctx, e.DeviceID)
	if err != nil {
		return false, err
	}
	return cur.Day == e.Day && cur.State == localdb.EvidenceAccepted, nil
}

// uploadDay sends a sealed day with the fields it was signed with and
// records the answer. Conflicts and rejections are recorded (and stop the
// chain); other failures are returned for backoff, leaving the day
// pending.
func (r *Runner) uploadDay(ctx context.Context, t serverapi.Target, e localdb.EvidenceDay) error {
	if !e.Provable() {
		return fmt.Errorf("evidence day %s has no bucket list and can't be sent", e.Day)
	}
	d := e.Record()
	err := r.opt.Server.UploadEvidenceDay(ctx, t, e.DeviceID, serverapi.EvidenceDayRequest{
		Day: e.Day, PrevChainHash: e.PrevChainHash, ContentHash: e.ContentHash, ChainHash: e.ChainHash,
		Signature: e.Signature, RulesVersion: e.RulesVersion, EventCounts: e.EventCounts,
		SeqFrom: d.SeqFrom, SeqTo: d.SeqTo, Buckets: d.Buckets, TimeFrom: d.TimeFrom, TimeTo: d.TimeTo,
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
