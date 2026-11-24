package evidence

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"slices"
	"time"
)

// Block is what hashing a block gives: the fields of its signed record
// (Day) that come from its rows.
type Block struct {
	// ContentHash is the hex Merkle root over the block's buckets.
	ContentHash string
	// Buckets is the leaf count: the repo bucket plus one per minute.
	Buckets int
	// TimeFrom and TimeTo are the first and last minute bucket's minute,
	// both nil when the block has only its repo bucket.
	TimeFrom, TimeTo *time.Time
	// Counts is how many rows of each type the block holds (Counts).
	Counts map[string]int
	// Rows is how many rows the block holds.
	Rows int
}

// Hasher hashes a block (ADR 0019) without holding it in memory. Rows go in
// with Add, in chunks, ordered by (BucketKey, Seq) across the whole block:
// first the repo rows, then each minute's rows. Each bucket gets a fresh
// 32-byte salt from the Hasher's salt source when it opens. Finish closes
// the block. Its state is small (the Merkle frontier and the open bucket's
// running hash) and can be saved (MarshalBinary) and restored
// (UnmarshalBinary), so one block can be hashed over several runs.
//
// Add and Finish return the buckets they close, in key order: the caller
// keeps that list (ADR 0019 point 3) to build proofs later. Together they
// return every bucket of the block exactly once.
type Hasher struct {
	salts io.Reader

	tree    frontier
	open    *openBucket
	started bool // the repo bucket has been opened
	any     bool // a row has been added
	lastKey uint64
	lastSeq int64
	// first and last are the first and last minute bucket's key, 0 when
	// there is none.
	first, last uint64
	counts      map[string]int
	rows        int
	done        bool
}

// openBucket is the bucket rows are being added to.
type openBucket struct {
	key  uint64
	salt Hash
	h    hash.Hash
}

// NewHasher returns a Hasher for an empty block that draws salts from
// salts: crypto/rand when nil. Tests pass a deterministic reader.
func NewHasher(salts io.Reader) *Hasher {
	return &Hasher{salts: salts, counts: Counts(nil)}
}

// ErrHasherDone is returned by a Hasher used after Finish.
var ErrHasherDone = errors.New("evidence: block already finished")

// Add hashes the next rows of the block. They must continue the block's
// (BucketKey, Seq) order: a row with a lower key than the last one added,
// or the same key and a sequence number at or below it, is rejected with
// ErrInvalidRow, like an invalid row, and nothing of the chunk is added.
// It returns the buckets the chunk closed.
func (h *Hasher) Add(rows []Row) ([]Bucket, error) {
	if h.done {
		return nil, ErrHasherDone
	}
	type entry struct {
		key  uint64
		line []byte
	}
	entries := make([]entry, len(rows))
	lastKey, lastSeq, anyRow := h.lastKey, h.lastSeq, h.any
	opens := 0 // buckets this chunk opens, each needing a salt
	started := h.started
	for i, r := range rows {
		key, err := BucketKey(r)
		if err != nil {
			return nil, err
		}
		if anyRow && (key < lastKey || key == lastKey && r.Seq <= lastSeq) {
			return nil, fmt.Errorf("%w: seq %d in bucket %d comes after seq %d in bucket %d", ErrInvalidRow, r.Seq, key, lastSeq, lastKey)
		}
		line, err := r.Line()
		if err != nil {
			return nil, err
		}
		if !anyRow || key != lastKey {
			if !started && key != RepoKey {
				opens++ // the empty repo bucket comes first
			}
			started = true
			opens++
		}
		entries[i] = entry{key: key, line: line}
		lastKey, lastSeq, anyRow = key, r.Seq, true
	}
	salts, err := h.drawSalts(opens)
	if err != nil {
		return nil, err
	}
	var closed []Bucket
	for i, e := range entries {
		if h.open == nil || h.open.key != e.key {
			if b, ok := h.closeOpen(); ok {
				closed = append(closed, b)
			}
			if !h.started && e.key != RepoKey {
				h.openBucket(RepoKey, salts[0])
				salts = salts[1:]
				b, _ := h.closeOpen()
				closed = append(closed, b)
			}
			h.openBucket(e.key, salts[0])
			salts = salts[1:]
		}
		h.open.h.Write(e.line)
		h.open.h.Write([]byte{'\n'})
		h.counts[rows[i].Type]++
		h.rows++
	}
	h.lastKey, h.lastSeq, h.any = lastKey, lastSeq, anyRow
	return closed, nil
}

// Finish closes the block and returns its summary and the buckets still
// open (the last one, or the empty repo bucket of a block with no rows).
// The Hasher can't be used after it.
func (h *Hasher) Finish() (Block, []Bucket, error) {
	if h.done {
		return Block{}, nil, ErrHasherDone
	}
	var closed []Bucket
	if !h.started {
		salts, err := h.drawSalts(1)
		if err != nil {
			return Block{}, nil, err
		}
		h.openBucket(RepoKey, salts[0])
	}
	if b, ok := h.closeOpen(); ok {
		closed = append(closed, b)
	}
	h.done = true
	blk := Block{
		ContentHash: h.tree.root().String(),
		Buckets:     int(h.tree.leaves()), //nolint:gosec // one leaf per stored minute, far below 2^63
		Counts:      maps.Clone(h.counts),
		Rows:        h.rows,
	}
	if h.first != 0 {
		from, to := KeyMinute(h.first), KeyMinute(h.last)
		blk.TimeFrom, blk.TimeTo = &from, &to
	}
	return blk, closed, nil
}

// drawSalts reads n salts.
func (h *Hasher) drawSalts(n int) ([]Hash, error) {
	src := h.salts
	if src == nil {
		src = rand.Reader
	}
	out := make([]Hash, n)
	for i := range out {
		if _, err := io.ReadFull(src, out[i][:]); err != nil {
			return nil, fmt.Errorf("evidence: drawing a bucket salt: %w", err)
		}
	}
	return out, nil
}

// openBucket starts a bucket.
func (h *Hasher) openBucket(key uint64, salt Hash) {
	d := sha256.New()
	d.Write([]byte{prefixBucket})
	d.Write(salt[:])
	h.open = &openBucket{key: key, salt: salt, h: d}
	h.started = true
	if key != RepoKey {
		if h.first == 0 {
			h.first = key
		}
		h.last = key
	}
}

// closeOpen closes the open bucket, if any, and adds its leaf.
func (h *Hasher) closeOpen() (Bucket, bool) {
	if h.open == nil {
		return Bucket{}, false
	}
	b := Bucket{Key: h.open.key, Salt: h.open.salt}
	h.open.h.Sum(b.Hash[:0])
	h.tree.add(leafHash(b.Key, b.Hash))
	h.open = nil
	return b, true
}

// hasherState is a Hasher's saved form.
type hasherState struct {
	Tree    frontier       `json:"tree"`
	Open    *openState     `json:"open,omitempty"`
	Started bool           `json:"started"`
	Any     bool           `json:"any"`
	LastKey uint64         `json:"last_key"`
	LastSeq int64          `json:"last_seq"`
	First   uint64         `json:"first"`
	Last    uint64         `json:"last"`
	Counts  map[string]int `json:"counts"`
	Rows    int            `json:"rows"`
}

type openState struct {
	Key    uint64 `json:"key"`
	Salt   Hash   `json:"salt"`
	SHA256 []byte `json:"sha256"`
}

// MarshalBinary saves h's state. A finished Hasher can't be saved.
func (h *Hasher) MarshalBinary() ([]byte, error) {
	if h.done {
		return nil, ErrHasherDone
	}
	st := hasherState{Tree: h.tree, Started: h.started, Any: h.any, LastKey: h.lastKey, LastSeq: h.lastSeq,
		First: h.first, Last: h.last, Counts: h.counts, Rows: h.rows}
	if h.open != nil {
		m, ok := h.open.h.(encoding.BinaryMarshaler)
		if !ok {
			return nil, errors.New("saving evidence hash: state can't be saved")
		}
		sum, err := m.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("saving evidence hash: %w", err)
		}
		st.Open = &openState{Key: h.open.key, Salt: h.open.salt, SHA256: sum}
	}
	b, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("saving evidence hash: %w", err)
	}
	return b, nil
}

// UnmarshalBinary restores a state saved by MarshalBinary. It keeps h's
// salt source, so restore into a Hasher made by NewHasher.
func (h *Hasher) UnmarshalBinary(b []byte) error {
	var st hasherState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("restoring evidence hash: %w", err)
	}
	// Before Finish, a bucket is open exactly when a row has been added.
	if !st.Tree.valid() || st.Rows < 0 || st.Started != st.Any || (st.Open != nil) != st.Any ||
		(st.Open != nil && st.Open.Key != st.LastKey) || st.First > st.Last {
		return errors.New("restoring evidence hash: inconsistent state")
	}
	counts := Counts(nil)
	for t, n := range st.Counts {
		if !slices.Contains(types, t) || n < 0 {
			return fmt.Errorf("restoring evidence hash: bad count %q=%d", t, n)
		}
		counts[t] = n
	}
	next := Hasher{salts: h.salts, tree: st.Tree, started: st.Started, any: st.Any, lastKey: st.LastKey,
		lastSeq: st.LastSeq, first: st.First, last: st.Last, counts: counts, rows: st.Rows}
	if st.Open != nil {
		d := sha256.New()
		u, ok := d.(encoding.BinaryUnmarshaler)
		if !ok {
			return errors.New("restoring evidence hash: state can't be restored")
		}
		if err := u.UnmarshalBinary(st.Open.SHA256); err != nil {
			return fmt.Errorf("restoring evidence hash: %w", err)
		}
		next.open = &openBucket{key: st.Open.Key, salt: st.Open.Salt, h: d}
	}
	*h = next
	return nil
}

// HashBlock hashes a whole block held in memory: rows in any order, with
// unique sequence numbers. It returns the block's summary and its bucket
// list in key order. Salts come from salts (crypto/rand when nil).
func HashBlock(rows []Row, salts io.Reader) (Block, []Bucket, error) {
	sorted, err := SortForHashing(rows)
	if err != nil {
		return Block{}, nil, err
	}
	h := NewHasher(salts)
	list, err := h.Add(sorted)
	if err != nil {
		return Block{}, nil, err
	}
	blk, rest, err := h.Finish()
	if err != nil {
		return Block{}, nil, err
	}
	return blk, append(list, rest...), nil
}

// SortForHashing validates rows and returns them in the order a Hasher
// takes them: by (BucketKey, Seq). A sequence number that appears twice is
// rejected with ErrInvalidRow.
func SortForHashing(rows []Row) ([]Row, error) {
	type keyed struct {
		key uint64
		row Row
	}
	ks := make([]keyed, len(rows))
	seen := make(map[int64]bool, len(rows))
	for i, r := range rows {
		k, err := BucketKey(r)
		if err != nil {
			return nil, err
		}
		if seen[r.Seq] {
			return nil, fmt.Errorf("%w: sequence number %d appears twice", ErrInvalidRow, r.Seq)
		}
		seen[r.Seq] = true
		ks[i] = keyed{key: k, row: r}
	}
	slices.SortFunc(ks, func(a, b keyed) int { return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.row.Seq, b.row.Seq)) })
	out := make([]Row, len(ks))
	for i, k := range ks {
		out[i] = k.row
	}
	return out, nil
}
