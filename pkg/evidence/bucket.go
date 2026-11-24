package evidence

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// Hash is a SHA-256 value, or a 32-byte salt. It encodes as lowercase hex
// in JSON and text.
type Hash [32]byte

// String returns h as lowercase hex.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// MarshalText encodes h as lowercase hex.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText decodes 64 hex digits.
func (h *Hash) UnmarshalText(b []byte) error {
	if len(b) != 2*len(h) {
		return fmt.Errorf("evidence: hash must be %d hex digits", 2*len(h))
	}
	if _, err := hex.Decode(h[:], b); err != nil {
		return fmt.Errorf("evidence: hash: %w", err)
	}
	return nil
}

// ParseHash decodes a lowercase-hex hash such as a content hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if !hexHash.MatchString(s) {
		return h, fmt.Errorf("evidence: %q is not a lowercase hex SHA-256", s)
	}
	err := h.UnmarshalText([]byte(s))
	return h, err
}

// RepoKey is the repo bucket's key. Every other bucket is a minute bucket
// keyed 1 + its Unix minute (MinuteKey).
const RepoKey uint64 = 0

// Hash prefixes (ADR 0019 point 1). Each kind of hashed value has its own,
// so a value of one kind can never pass for another.
const (
	prefixLeaf   = 0x00
	prefixNode   = 0x01
	prefixBucket = 0x02
)

// Bucket is one leaf of a block: its key, its salt and the hash of its
// lines. The agent keeps the list of a sealed block's buckets (ADR 0019
// point 3), so it can prove the buckets it hides after their rows are
// pruned.
type Bucket struct {
	Key  uint64 `json:"key"`
	Salt Hash   `json:"salt"`
	Hash Hash   `json:"bucket_hash"`
}

// MinuteKey returns the key of the minute bucket holding t: 1 + the Unix
// minute of t. Times before 1970 have no minute bucket and give 0, the repo
// bucket's key; BucketKey rejects rows there.
func MinuteKey(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	return 1 + uint64(s/60) //nolint:gosec // s is not negative
}

// KeyMinute returns the start of the minute of a minute bucket's key (key
// >= 1), in UTC.
func KeyMinute(key uint64) time.Time {
	return time.Unix(int64(key-1)*60, 0).UTC() //nolint:gosec // keys come from MinuteKey, far below 2^63
}

// BucketKey returns the key of the bucket r belongs in: RepoKey for a repo
// row, whatever its time, and the minute bucket of r.Time for every other
// row.
func BucketKey(r Row) (uint64, error) {
	if err := r.validate(); err != nil {
		return 0, err
	}
	if r.Type == TypeRepo {
		return RepoKey, nil
	}
	k := MinuteKey(r.Time)
	if k == RepoKey {
		return 0, fmt.Errorf("%w: seq %d is timed before 1970", ErrInvalidRow, r.Seq)
	}
	return k, nil
}

// bucketHash is SHA-256(0x02 ‖ salt ‖ lines), each line followed by "\n".
func bucketHash(salt Hash, lines [][]byte) Hash {
	h := sha256.New()
	h.Write([]byte{prefixBucket})
	h.Write(salt[:])
	for _, l := range lines {
		h.Write(l)
		h.Write([]byte{'\n'})
	}
	var out Hash
	h.Sum(out[:0])
	return out
}

// leafHash is SHA-256(0x00 ‖ key as 8-byte big-endian ‖ bucket_hash).
func leafHash(key uint64, bucket Hash) Hash {
	var b [1 + 8 + 32]byte
	b[0] = prefixLeaf
	binary.BigEndian.PutUint64(b[1:9], key)
	copy(b[9:], bucket[:])
	return sha256.Sum256(b[:])
}

// nodeHash is SHA-256(0x01 ‖ left ‖ right).
func nodeHash(left, right Hash) Hash {
	var b [1 + 32 + 32]byte
	b[0] = prefixNode
	copy(b[1:33], left[:])
	copy(b[33:], right[:])
	return sha256.Sum256(b[:])
}

// splitPoint is where RFC 6962 splits n > 1 leaves: the largest power of
// two smaller than n.
func splitPoint(n int) int {
	k := 1
	for k<<1 < n {
		k <<= 1
	}
	return k
}

// rootOf is the RFC 6962 root of leaves (at least one).
func rootOf(leaves []Hash) Hash {
	if len(leaves) == 1 {
		return leaves[0]
	}
	k := splitPoint(len(leaves))
	return nodeHash(rootOf(leaves[:k]), rootOf(leaves[k:]))
}

// Root returns the content hash of a block from its bucket list, which must
// be sorted by key: the RFC 6962 root over the buckets' leaves. The agent
// uses it to check a stored bucket list against the signed content hash.
func Root(list []Bucket) (Hash, error) {
	if len(list) == 0 || list[0].Key != RepoKey {
		return Hash{}, fmt.Errorf("%w: a bucket list starts with the repo bucket", ErrInvalidProof)
	}
	leaves := make([]Hash, len(list))
	for i, b := range list {
		if i > 0 && b.Key <= list[i-1].Key {
			return Hash{}, fmt.Errorf("%w: bucket keys must increase", ErrInvalidProof)
		}
		leaves[i] = leafHash(b.Key, b.Hash)
	}
	return rootOf(leaves), nil
}

// frontier is a Merkle tree being built one leaf at a time: the roots of
// its perfect subtrees, largest (leftmost) first. Their sizes are the
// binary digits of the leaf count.
type frontier struct {
	Nodes []frontierNode `json:"nodes"`
}

type frontierNode struct {
	Size uint64 `json:"size"`
	Hash Hash   `json:"hash"`
}

// add appends a leaf, merging equal-sized subtrees.
func (f *frontier) add(leaf Hash) {
	f.Nodes = append(f.Nodes, frontierNode{Size: 1, Hash: leaf})
	for n := len(f.Nodes); n > 1 && f.Nodes[n-2].Size == f.Nodes[n-1].Size; n = len(f.Nodes) {
		merged := frontierNode{Size: 2 * f.Nodes[n-1].Size, Hash: nodeHash(f.Nodes[n-2].Hash, f.Nodes[n-1].Hash)}
		f.Nodes = append(f.Nodes[:n-2], merged)
	}
}

// root folds the subtrees right to left, which is RFC 6962's shape: the
// leftmost subtree is the largest power of two below the count.
func (f *frontier) root() Hash {
	r := f.Nodes[len(f.Nodes)-1].Hash
	for i := len(f.Nodes) - 2; i >= 0; i-- {
		r = nodeHash(f.Nodes[i].Hash, r)
	}
	return r
}

// leaves is the leaf count.
func (f *frontier) leaves() uint64 {
	var n uint64
	for _, x := range f.Nodes {
		n += x.Size
	}
	return n
}

// valid checks a restored frontier: strictly decreasing powers of two.
func (f *frontier) valid() bool {
	for i, x := range f.Nodes {
		if x.Size == 0 || x.Size&(x.Size-1) != 0 || (i > 0 && x.Size >= f.Nodes[i-1].Size) {
			return false
		}
	}
	return true
}
