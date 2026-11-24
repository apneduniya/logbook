package evidence

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// MinuteRange is the half-open range of minutes [From, To). A bound that
// isn't a whole minute widens the range to the whole minute it is in.
type MinuteRange struct {
	From, To time.Time
}

// keys returns the range's minute bucket keys [a, b); a >= b when the
// range holds no minute at or after 1970.
func (r MinuteRange) keys() (a, b uint64) {
	a = max(MinuteKey(r.From), 1)
	to := r.To.Truncate(time.Minute)
	if !to.Equal(r.To) {
		to = to.Add(time.Minute)
	}
	return a, MinuteKey(to)
}

// NeedsCompleteness reports whether a proof for the block d must show it
// revealed every row of r that a run at high-water mark h read (ADR 0019
// point 6): only when d's signed time span overlaps r and the block starts
// below h. Every other block is known from its signed fields to hold no
// such row.
func NeedsCompleteness(d Day, r MinuteRange, h int64) bool {
	if d.TimeFrom == nil || d.TimeTo == nil || d.SeqFrom >= h {
		return false
	}
	a, b := r.keys()
	return a < b && MinuteKey(*d.TimeFrom) < b && MinuteKey(*d.TimeTo) >= a
}

// PlanProof says which buckets of a block (its bucket list, sorted by key)
// a proof must reveal to show it holds nothing else in ranges: open every
// minute bucket with a key in a range, and reveal as edges the leaves just
// before and just after each range (none after one that reaches the last
// leaf). The repo bucket is always before every range; the caller adds
// index 0 to open when it needs the repo mapping. Both lists are sorted,
// and an index in open is never in edges.
func PlanProof(list []Bucket, ranges []MinuteRange) (open, edges []int) {
	opened := map[int]bool{}
	edge := map[int]bool{}
	for _, r := range ranges {
		a, b := r.keys()
		if a >= b || len(list) == 0 {
			continue
		}
		lo := sort.Search(len(list), func(i int) bool { return list[i].Key >= a })
		hi := sort.Search(len(list), func(i int) bool { return list[i].Key >= b })
		for i := lo; i < hi; i++ {
			opened[i] = true
		}
		if lo > 0 {
			edge[lo-1] = true
		}
		if hi < len(list) {
			edge[hi] = true
		}
	}
	for i := range opened {
		open = append(open, i)
		delete(edge, i)
	}
	for i := range edge {
		edges = append(edges, i)
	}
	slices.Sort(open)
	slices.Sort(edges)
	return open, edges
}

// ErrIncomplete means a verified proof doesn't show that it revealed every
// bucket of a range.
var ErrIncomplete = fmt.Errorf("%w: a range is not proven complete", ErrInvalidProof)

// CheckComplete checks that the verified proof revealed every row the block
// holds in r: some listed leaf before the range has a key before it, and
// every leaf from there to the first listed leaf with a key at or after
// the range's end (or to the last leaf) is opened. Keys increase with the
// index, so no bucket of the range can hide between them.
func (v Verified) CheckComplete(r MinuteRange) error {
	a, b := r.keys()
	if a >= b {
		return nil
	}
	type leaf struct {
		i      int
		key    uint64
		opened bool
	}
	var listed []leaf
	for _, o := range v.Opened {
		listed = append(listed, leaf{o.I, o.Key, true})
	}
	for _, e := range v.Edges {
		listed = append(listed, leaf{e.I, e.Key, false})
	}
	slices.SortFunc(listed, func(x, y leaf) int { return x.i - y.i })
	left := -1 // position in listed of the last leaf with a key before a
	for p, l := range listed {
		if l.key < a {
			left = p
		}
	}
	if left < 0 {
		return fmt.Errorf("%w: no leaf bounds %s on the left", ErrIncomplete, r.From.UTC().Format(time.RFC3339))
	}
	end := v.Buckets // index of the first leaf at or after b, or n
	for _, l := range listed[left+1:] {
		if l.key >= b {
			end = l.i
			break
		}
	}
	want := listed[left].i + 1
	for _, l := range listed[left+1:] {
		if l.i >= end {
			break
		}
		if l.i != want || !l.opened {
			return fmt.Errorf("%w: leaf %d of %s..%s is not opened", ErrIncomplete, want,
				r.From.UTC().Format(time.RFC3339), r.To.UTC().Format(time.RFC3339))
		}
		want++
	}
	if want != end {
		return fmt.Errorf("%w: leaf %d of %s..%s is not opened", ErrIncomplete, want,
			r.From.UTC().Format(time.RFC3339), r.To.UTC().Format(time.RFC3339))
	}
	return nil
}
