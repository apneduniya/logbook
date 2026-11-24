package evidence

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

var (
	// ErrInvalidProof is wrapped by every reason a proof doesn't verify.
	ErrInvalidProof = errors.New("invalid evidence proof")
	// ErrBucketIncomplete means rows given to open a bucket don't hash to
	// its stored bucket hash: some were pruned, or they aren't its rows.
	// The minutes that need the bucket go without evidence (unbacked).
	ErrBucketIncomplete = errors.New("evidence bucket rows don't match its hash")
)

// Proof reveals some buckets of one sealed block (ADR 0019 point 5) and
// hides the rest.
type Proof struct {
	// Buckets is the block's leaf count n.
	Buckets int `json:"buckets"`
	// Opened are the buckets revealed in full, by increasing index.
	Opened []OpenedBucket `json:"opened"`
	// Edges are leaves revealed only by key and bucket hash, to bound a
	// range, by increasing index.
	Edges []EdgeBucket `json:"edges"`
	// Hashes are the hashes of every maximal subtree holding no opened or
	// edge leaf, left to right.
	Hashes []Hash `json:"hashes"`
}

// OpenedBucket is a bucket revealed in full: I is its 0-based leaf index,
// Lines its canonical lines in sequence order, without "\n".
type OpenedBucket struct {
	I     int      `json:"i"`
	Key   uint64   `json:"key"`
	Salt  Hash     `json:"salt"`
	Lines []string `json:"lines"`
}

// EdgeBucket is a leaf revealed only by its key and bucket hash.
type EdgeBucket struct {
	I          int    `json:"i"`
	Key        uint64 `json:"key"`
	BucketHash Hash   `json:"bucket_hash"`
}

// BuildProof builds the proof for a block from its bucket list (sorted by
// key, as the Hasher returns it). open maps the index of each bucket to
// reveal to its rows, in any order; they must hash to the bucket's stored
// hash, or BuildProof fails with ErrBucketIncomplete (rows pruned since
// sealing). edges are the indexes of leaves to reveal by key and hash; one
// also in open is opened instead. PlanProof says which to pass for a set of
// minute ranges.
func BuildProof(list []Bucket, open map[int][]Row, edges []int) (Proof, error) {
	if _, err := Root(list); err != nil {
		return Proof{}, err
	}
	p := Proof{Buckets: len(list)}
	listed := make(map[int]bool, len(open)+len(edges))
	for i, rows := range open {
		if i < 0 || i >= len(list) {
			return Proof{}, fmt.Errorf("%w: bucket %d of %d", ErrInvalidProof, i, len(list))
		}
		b := list[i]
		lines, err := b.lines(rows)
		if err != nil {
			return Proof{}, fmt.Errorf("bucket %d: %w", i, err)
		}
		text := make([]string, len(lines))
		for j, l := range lines {
			text[j] = string(l)
		}
		p.Opened = append(p.Opened, OpenedBucket{I: i, Key: b.Key, Salt: b.Salt, Lines: text})
		listed[i] = true
	}
	for _, i := range edges {
		if i < 0 || i >= len(list) {
			return Proof{}, fmt.Errorf("%w: edge %d of %d", ErrInvalidProof, i, len(list))
		}
		if listed[i] {
			continue
		}
		p.Edges = append(p.Edges, EdgeBucket{I: i, Key: list[i].Key, BucketHash: list[i].Hash})
		listed[i] = true
	}
	slices.SortFunc(p.Opened, func(a, b OpenedBucket) int { return cmp.Compare(a.I, b.I) })
	slices.SortFunc(p.Edges, func(a, b EdgeBucket) int { return cmp.Compare(a.I, b.I) })

	leaves := make([]Hash, len(list))
	for i, b := range list {
		leaves[i] = leafHash(b.Key, b.Hash)
	}
	idx := slices.Sorted(func(yield func(int) bool) {
		for i := range listed {
			if !yield(i) {
				return
			}
		}
	})
	var walk func(lo, hi int)
	walk = func(lo, hi int) {
		if !anyIn(idx, lo, hi) {
			p.Hashes = append(p.Hashes, rootOf(leaves[lo:hi]))
			return
		}
		if hi-lo == 1 {
			return // a listed leaf: the verifier hashes it itself
		}
		k := splitPoint(hi - lo)
		walk(lo, lo+k)
		walk(lo+k, hi)
	}
	walk(0, len(list))
	return p, nil
}

// anyIn reports whether sorted idx has a value in [lo, hi).
func anyIn(idx []int, lo, hi int) bool {
	i, _ := slices.BinarySearch(idx, lo)
	return i < len(idx) && idx[i] < hi
}

// Verified is what a proof showed: the rows of its opened buckets, each
// checked against the signed record, and which leaves it listed.
type Verified struct {
	// Buckets is the block's leaf count.
	Buckets int
	// Opened are the opened buckets, by increasing index.
	Opened []VerifiedBucket
	// Edges are the edge leaves, by increasing index.
	Edges []EdgeBucket
}

// VerifiedBucket is an opened bucket's decoded rows, in sequence order.
type VerifiedBucket struct {
	I    int
	Key  uint64
	Rows []Row
}

// Rows returns the rows of every opened bucket, bucket by bucket.
func (v Verified) Rows() []Row {
	var out []Row
	for _, b := range v.Opened {
		out = append(out, b.Rows...)
	}
	return out
}

// VerifyProof checks p against the signed record d of its block (ADR 0019
// points 5 and 6): the leaf count is d's; the walk from the root uses every
// hash exactly once and reaches d's content hash; leaf keys increase with
// the index, with the repo bucket (key 0) exactly at index 0, and minute
// keys inside d's time span; every opened line is a canonical row with a
// sequence number in (d.SeqFrom, d.SeqTo], increasing within its bucket,
// in the right bucket (repo rows only in the repo bucket, every other row
// only in the bucket of its minute); and no opened minute bucket is empty.
// d's other fields are the caller's to check (its signature and chain).
func VerifyProof(p Proof, d Day) (Verified, error) {
	root, err := ParseHash(d.ContentHash)
	if err != nil {
		return Verified{}, fmt.Errorf("%w: %w", ErrInvalidProof, err)
	}
	// One leaf per minute fits in 2^32 for 8,000 years; the bound keeps the
	// tree arithmetic far from overflow.
	if p.Buckets < 1 || int64(p.Buckets) > 1<<32 || p.Buckets != d.Buckets {
		return Verified{}, fmt.Errorf("%w: %d buckets, the record has %d", ErrInvalidProof, p.Buckets, d.Buckets)
	}
	leaves := make(map[int]Hash, len(p.Opened)+len(p.Edges))
	keys := make(map[int]uint64, len(p.Opened)+len(p.Edges))
	v := Verified{Buckets: p.Buckets, Edges: slices.Clone(p.Edges)}
	for _, o := range p.Opened {
		if _, dup := leaves[o.I]; dup || o.I < 0 || o.I >= p.Buckets {
			return Verified{}, fmt.Errorf("%w: opened bucket index %d", ErrInvalidProof, o.I)
		}
		rows, lines, err := openedRows(o, d)
		if err != nil {
			return Verified{}, err
		}
		leaves[o.I], keys[o.I] = leafHash(o.Key, bucketHash(o.Salt, lines)), o.Key
		v.Opened = append(v.Opened, VerifiedBucket{I: o.I, Key: o.Key, Rows: rows})
	}
	for _, e := range p.Edges {
		if _, dup := leaves[e.I]; dup || e.I < 0 || e.I >= p.Buckets {
			return Verified{}, fmt.Errorf("%w: edge index %d", ErrInvalidProof, e.I)
		}
		leaves[e.I], keys[e.I] = leafHash(e.Key, e.BucketHash), e.Key
	}
	idx := slices.Sorted(func(yield func(int) bool) {
		for i := range leaves {
			if !yield(i) {
				return
			}
		}
	})
	if err := checkKeys(idx, keys, d); err != nil {
		return Verified{}, err
	}
	next := 0
	var walk func(lo, hi int) (Hash, error)
	walk = func(lo, hi int) (Hash, error) {
		if !anyIn(idx, lo, hi) {
			if next >= len(p.Hashes) {
				return Hash{}, fmt.Errorf("%w: too few subtree hashes", ErrInvalidProof)
			}
			next++
			return p.Hashes[next-1], nil
		}
		if hi-lo == 1 {
			return leaves[lo], nil
		}
		k := splitPoint(hi - lo)
		l, err := walk(lo, lo+k)
		if err != nil {
			return Hash{}, err
		}
		r, err := walk(lo+k, hi)
		if err != nil {
			return Hash{}, err
		}
		return nodeHash(l, r), nil
	}
	got, err := walk(0, p.Buckets)
	if err != nil {
		return Verified{}, err
	}
	if next != len(p.Hashes) {
		return Verified{}, fmt.Errorf("%w: %d subtree hashes unused", ErrInvalidProof, len(p.Hashes)-next)
	}
	if got != root {
		return Verified{}, fmt.Errorf("%w: root %s, content hash %s", ErrInvalidProof, got, root)
	}
	slices.SortFunc(v.Opened, func(a, b VerifiedBucket) int { return cmp.Compare(a.I, b.I) })
	slices.SortFunc(v.Edges, func(a, b EdgeBucket) int { return cmp.Compare(a.I, b.I) })
	return v, nil
}

// openedRows decodes and checks an opened bucket's lines.
func openedRows(o OpenedBucket, d Day) ([]Row, [][]byte, error) {
	if o.Key != RepoKey && len(o.Lines) == 0 {
		return nil, nil, fmt.Errorf("%w: minute bucket %d is empty", ErrInvalidProof, o.Key)
	}
	rows := make([]Row, len(o.Lines))
	lines := make([][]byte, len(o.Lines))
	for j, text := range o.Lines {
		lines[j] = []byte(text)
		r, err := DecodeLine(lines[j])
		if err != nil {
			return nil, nil, fmt.Errorf("%w: bucket %d: %w", ErrInvalidProof, o.I, err)
		}
		if r.Seq <= d.SeqFrom || r.Seq > d.SeqTo {
			return nil, nil, fmt.Errorf("%w: seq %d outside the block's (%d, %d]", ErrInvalidProof, r.Seq, d.SeqFrom, d.SeqTo)
		}
		if j > 0 && r.Seq <= rows[j-1].Seq {
			return nil, nil, fmt.Errorf("%w: bucket %d: seq %d after %d", ErrInvalidProof, o.I, r.Seq, rows[j-1].Seq)
		}
		k, err := BucketKey(r)
		if err != nil || k != o.Key {
			return nil, nil, fmt.Errorf("%w: seq %d is not a row of bucket %d", ErrInvalidProof, r.Seq, o.Key)
		}
		rows[j] = r
	}
	return rows, lines, nil
}

// checkKeys checks the listed leaves' keys: increasing with the index, the
// repo key exactly at index 0, and minute keys inside the signed span.
func checkKeys(idx []int, keys map[int]uint64, d Day) error {
	lo, hi := uint64(1), uint64(0) // an empty span: no minute bucket allowed
	if d.TimeFrom != nil && d.TimeTo != nil {
		lo, hi = MinuteKey(*d.TimeFrom), MinuteKey(*d.TimeTo)
	}
	for n, i := range idx {
		k := keys[i]
		if (k == RepoKey) != (i == 0) {
			return fmt.Errorf("%w: key %d at index %d", ErrInvalidProof, k, i)
		}
		if k != RepoKey && (k < lo || k > hi) {
			return fmt.Errorf("%w: minute key %d outside the block's time span", ErrInvalidProof, k)
		}
		// Keys are distinct and increase with the index, so between two
		// leaves there are at least as many keys as indexes.
		if n > 0 && (k <= keys[idx[n-1]] || k-keys[idx[n-1]] < uint64(i-idx[n-1])) { //nolint:gosec // idx is sorted
			return fmt.Errorf("%w: keys don't increase at index %d", ErrInvalidProof, i)
		}
	}
	return nil
}
