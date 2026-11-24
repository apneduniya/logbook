package crediting

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
)

// BlockProof is a proof (ADR 0019 point 5) for one block of the device's
// chain, with the record the block was signed with.
type BlockProof struct {
	Record    evidence.Day
	ChainHash string
	Proof     evidence.Proof
}

// Proofs is what BuildProofs gives for a set of computation keys.
type Proofs struct {
	// Blocks are the proofs, oldest block first. Together they reveal, for
	// every backed key, every row a run at the key read in its window
	// [T - 120 min, T) and the repo bucket of the block holding H.
	Blocks []BlockProof
	// Unbacked are the keys, in the order given, that no proof backs: the
	// block holding H isn't sealed and accepted yet, or doesn't open with a
	// repo snapshot (it follows a day sealed before the agent kept bucket
	// lists), rows the run read lie outside the device's provable blocks
	// (before its chain started, or in such a day), or a bucket that must
	// be opened has lost rows to pruning. Their minutes go without
	// evidence.
	Unbacked []replay.Key
}

// BuildProofs builds, from the local log and the stored bucket lists, the
// proofs a submission uploads for the minutes computed at keys (ADR 0020):
// for each accepted block whose signed span overlaps a key's window and
// that starts below the key's H, every minute bucket of the window with
// the leaves just outside it (completeness, ADR 0019 point 6); and the
// repo bucket of the block holding each key's H. Hidden buckets need only
// the bucket list, so pruning their rows doesn't matter.
func BuildProofs(ctx context.Context, log LogReader, deviceID string, keys []replay.Key) (Proofs, error) {
	for _, k := range keys {
		if err := k.Validate(); err != nil {
			return Proofs{}, err
		}
	}
	var out Proofs
	backed := make([]bool, len(keys))
	err := log.View(ctx, func(s *localdb.Snapshot) error {
		blocks, err := provableBlocks(ctx, s, deviceID)
		if err != nil {
			return err
		}
		for i, k := range keys {
			if backed[i], err = covered(ctx, s, blocks, k); err != nil {
				return err
			}
		}
		// Read the buckets every backed key needs, then drop keys that
		// need one that has lost rows.
		for _, b := range blocks {
			if err := b.read(ctx, s, keys, backed); err != nil {
				return err
			}
		}
		for i, k := range keys {
			for _, b := range blocks {
				if backed[i] && !b.complete(k) {
					backed[i] = false
				}
			}
		}
		for _, b := range blocks {
			p, ok, err := b.proof(keys, backed)
			if err != nil {
				return err
			}
			if ok {
				out.Blocks = append(out.Blocks, p)
			}
		}
		return nil
	})
	if err != nil {
		return Proofs{}, fmt.Errorf("building evidence proofs: %w", err)
	}
	for i, k := range keys {
		if !backed[i] {
			out.Unbacked = append(out.Unbacked, k)
		}
	}
	return out, nil
}

// sealedBlock is a provable block of the chain with its bucket list, and
// the rows of the buckets read for proofs, by key.
type sealedBlock struct {
	day  localdb.EvidenceDay
	rec  evidence.Day
	list []evidence.Bucket
	// mapped is true when the block's repo bucket holds the whole mapping
	// at every H in its range (see provableBlocks).
	mapped bool
	rows   map[uint64][]evidence.Row
	// bad are the indexes of read buckets whose rows don't hash to them.
	bad map[int]bool
}

// provableBlocks returns the device's accepted blocks that have a bucket
// list, in chain order.
//
// A block's repo bucket holds the whole mapping (ADR 0019 point 4) when
// the block is the device's first (it starts at 0, or at the takeover's
// snapshot), follows a legacy day (it starts again at 0), or follows a
// block sealed with a bucket list (whose reservation logged the snapshot
// that opens it). A block that follows one sealed before the agent kept
// bucket lists has no snapshot: paths mapped before it started are missing
// from its repo bucket.
func provableBlocks(ctx context.Context, s *localdb.Snapshot, deviceID string) ([]*sealedBlock, error) {
	days, err := s.EvidenceDays(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	var out []*sealedBlock
	for i, d := range days {
		if d.State != localdb.EvidenceAccepted || !d.Provable() {
			continue
		}
		mapped := i == 0 || days[i-1].Legacy || days[i-1].Provable()
		list, err := s.Buckets(ctx, deviceID, d.Day)
		if err != nil {
			return nil, err
		}
		if len(list) != d.Buckets {
			return nil, fmt.Errorf("%s has %d stored buckets, signed %d", d.Day, len(list), d.Buckets)
		}
		out = append(out, &sealedBlock{day: d, rec: d.Record(), list: list, mapped: mapped})
	}
	slices.SortFunc(out, func(a, b *sealedBlock) int { return cmp.Compare(a.rec.SeqFrom, b.rec.SeqFrom) })
	return out, nil
}

// window is the minutes a run at k read: [T - 120 min, T).
func window(k replay.Key) evidence.MinuteRange {
	sp := k.Span()
	return evidence.MinuteRange{From: sp.History, To: sp.To}
}

// holds reports whether the block d holds sequence number h.
func holds(d evidence.Day, h int64) bool { return d.SeqFrom < h && h <= d.SeqTo }

// covered reports whether provable blocks can back a run at k: one of them
// holds k.H and its repo bucket holds the whole mapping, and no row the run
// read (an engine event of its window with a sequence number at or below
// k.H) lies outside them.
func covered(ctx context.Context, s *localdb.Snapshot, blocks []*sealedBlock, k replay.Key) (bool, error) {
	if !slices.ContainsFunc(blocks, func(b *sealedBlock) bool { return b.mapped && holds(b.rec, k.H) }) {
		return false, nil
	}
	w := window(k)
	from := int64(0)
	for _, b := range blocks {
		if b.rec.SeqFrom >= k.H {
			break
		}
		if b.rec.SeqFrom > from {
			ev, err := s.Events(ctx, w.From, w.To, from, b.rec.SeqFrom)
			if err != nil {
				return false, err
			}
			if len(ev.Activity)+len(ev.Heartbeats)+len(ev.AI) > 0 {
				return false, nil
			}
		}
		from = b.rec.SeqTo
	}
	return true, nil
}

// ranges returns the windows of the backed keys for which b must show
// completeness, and whether b holds some backed key's H (so its repo
// bucket is opened).
func (b *sealedBlock) ranges(keys []replay.Key, backed []bool) (rs []evidence.MinuteRange, repo bool) {
	for i, k := range keys {
		if !backed[i] {
			continue
		}
		if r := window(k); evidence.NeedsCompleteness(b.rec, r, k.H) {
			rs = append(rs, r)
		}
		repo = repo || holds(b.rec, k.H)
	}
	return rs, repo
}

// read reads the rows of every bucket b opens for the backed keys and
// checks them against the bucket list.
func (b *sealedBlock) read(ctx context.Context, s *localdb.Snapshot, keys []replay.Key, backed []bool) error {
	rs, repo := b.ranges(keys, backed)
	b.rows, b.bad = map[uint64][]evidence.Row{}, map[int]bool{}
	var rows []evidence.Row
	for _, r := range mergeRanges(rs) {
		got, err := s.MinuteRows(ctx, b.rec.SeqFrom, b.rec.SeqTo, r.From, r.To)
		if err != nil {
			return err
		}
		rows = append(rows, got...)
	}
	if repo {
		got, err := s.RepoRows(ctx, b.rec.SeqFrom, b.rec.SeqTo)
		if err != nil {
			return err
		}
		rows = append(rows, got...)
	}
	for _, r := range rows {
		k, err := evidence.BucketKey(r)
		if err != nil {
			return err
		}
		b.rows[k] = append(b.rows[k], r)
	}
	open, _ := evidence.PlanProof(b.list, rs)
	if repo {
		open = append(open, 0)
	}
	for _, i := range open {
		err := b.list[i].Check(b.rows[b.list[i].Key])
		switch {
		case errors.Is(err, evidence.ErrBucketIncomplete):
			b.bad[i] = true
		case err != nil:
			return err
		}
	}
	return nil
}

// complete reports whether every bucket of b a run at k needs is intact.
func (b *sealedBlock) complete(k replay.Key) bool {
	var rs []evidence.MinuteRange
	if r := window(k); evidence.NeedsCompleteness(b.rec, r, k.H) {
		rs = append(rs, r)
	}
	open, _ := evidence.PlanProof(b.list, rs)
	if holds(b.rec, k.H) {
		open = append(open, 0)
	}
	return !slices.ContainsFunc(open, func(i int) bool { return b.bad[i] })
}

// proof builds b's proof for the backed keys; ok is false when they need
// nothing of b.
func (b *sealedBlock) proof(keys []replay.Key, backed []bool) (BlockProof, bool, error) {
	rs, repo := b.ranges(keys, backed)
	if len(rs) == 0 && !repo {
		return BlockProof{}, false, nil
	}
	open, edges := evidence.PlanProof(b.list, rs)
	if repo {
		open = append(open, 0)
		edges = slices.DeleteFunc(edges, func(i int) bool { return i == 0 })
	}
	opened := make(map[int][]evidence.Row, len(open))
	for _, i := range open {
		opened[i] = b.rows[b.list[i].Key]
	}
	p, err := evidence.BuildProof(b.list, opened, edges)
	if err != nil {
		return BlockProof{}, false, fmt.Errorf("proving %s: %w", b.day.Day, err)
	}
	return BlockProof{Record: b.rec, ChainHash: b.day.ChainHash, Proof: p}, true, nil
}

// mergeRanges returns rs sorted, with overlapping or touching ranges joined.
func mergeRanges(rs []evidence.MinuteRange) []evidence.MinuteRange {
	rs = slices.Clone(rs)
	slices.SortFunc(rs, func(a, b evidence.MinuteRange) int { return a.From.Compare(b.From) })
	var out []evidence.MinuteRange
	for _, r := range rs {
		if n := len(out); n > 0 && !r.From.After(out[n-1].To) {
			if r.To.After(out[n-1].To) {
				out[n-1].To = r.To
			}
			continue
		}
		out = append(out, r)
	}
	return out
}
