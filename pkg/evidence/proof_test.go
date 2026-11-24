package evidence

import (
	"cmp"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// sealed hashes rows as a block with seq range (0, max seq] and returns
// its record, bucket list and rows by bucket index.
func sealed(t testing.TB, rows []Row, salts func() *fixedSalts) (Day, []Bucket, map[int][]Row) {
	t.Helper()
	blk, list, err := HashBlock(rows, salts())
	if err != nil {
		t.Fatal(err)
	}
	d := Day{ContentHash: blk.ContentHash, Buckets: blk.Buckets, TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo, SeqTo: 1}
	for _, r := range rows {
		d.SeqTo = max(d.SeqTo, r.Seq)
	}
	byIndex := map[int][]Row{}
	for _, r := range rows {
		k, _ := BucketKey(r)
		i, _ := slices.BinarySearchFunc(list, k, func(b Bucket, k uint64) int { return cmp.Compare(b.Key, k) })
		byIndex[i] = append(byIndex[i], r)
	}
	return d, list, byIndex
}

func newSalts() *fixedSalts { return &fixedSalts{} }

func opening(byIndex map[int][]Row, idx []int) map[int][]Row {
	out := map[int][]Row{}
	for _, i := range idx {
		out[i] = byIndex[i]
	}
	return out
}

// minuteOf is the fixture minute 09:mm on the 19th.
func minuteOf(mm int) time.Time { return time.Date(2026, 11, 19, 9, mm, 0, 0, time.UTC) }

func TestProofRoundTrip(t *testing.T) {
	d, list, byIndex := sealed(t, fixtureBlock(), newSalts)
	r := MinuteRange{From: minuteOf(0), To: minuteOf(2)}
	open, edges := PlanProof(list, []MinuteRange{r})
	open = append([]int{0}, open...) // and the repo bucket
	p, err := BuildProof(list, opening(byIndex, open), edges)
	if err != nil {
		t.Fatal(err)
	}
	v, err := VerifyProof(p, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.CheckComplete(r); err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, row := range v.Rows() {
		seqs = append(seqs, row.Seq)
	}
	// The repo rows, then 09:00 (seq 1, 3, 4, 5, 6, 9, 12; active time 7
	// ends at 09:01 and is in that minute's bucket).
	if want := []int64{2, 8, 1, 3, 4, 5, 6, 9, 12, 7}; !slices.Equal(seqs, want) {
		t.Fatalf("revealed seqs %v, want %v", seqs, want)
	}
	// Bounded by the previous minute bucket (an event of the 18th) and 09:30.
	if len(p.Edges) != 2 || p.Edges[0].Key != MinuteKey(minuteOf(-600+1)) || p.Edges[1].Key != MinuteKey(minuteOf(30)) {
		t.Fatalf("edges = %+v, want 23:01 the day before and 09:30", p.Edges)
	}
}

// Every way of tampering with a proof makes it fail.
func TestProofTampering(t *testing.T) {
	// More minutes after 09:30, so the proof has subtree hashes too.
	rows := fixtureBlock()
	for i := range 16 {
		rows = append(rows, ActivityRow(int64(20+i), wire.ActivitySample{Time: minuteOf(40 + i), FocusKind: "editor"}))
	}
	d, list, byIndex := sealed(t, rows, newSalts)
	r := MinuteRange{From: minuteOf(0), To: minuteOf(2)}
	open, edges := PlanProof(list, []MinuteRange{r})
	build := func() Proof {
		p, err := BuildProof(list, opening(byIndex, append([]int{0}, open...)), edges)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name   string
		mutate func(*Proof, *Day)
	}{
		{"changed row", func(p *Proof, _ *Day) {
			p.Opened[1].Lines[0] = strings.Replace(p.Opened[1].Lines[0], `"idle_seconds":0`, `"idle_seconds":1`, 1)
		}},
		{"dropped row", func(p *Proof, _ *Day) { p.Opened[1].Lines = p.Opened[1].Lines[1:] }},
		{"swapped rows", func(p *Proof, _ *Day) {
			l := p.Opened[1].Lines
			l[0], l[1] = l[1], l[0]
		}},
		{"salt", func(p *Proof, _ *Day) { p.Opened[1].Salt[0]++ }},
		{"missing opened bucket", func(p *Proof, _ *Day) { p.Opened = p.Opened[1:] }},
		{"missing edge", func(p *Proof, _ *Day) { p.Edges = nil }},
		{"edge hash", func(p *Proof, _ *Day) { p.Edges[0].BucketHash[0]++ }},
		{"edge key", func(p *Proof, _ *Day) { p.Edges[0].Key++ }},
		{"opened key", func(p *Proof, _ *Day) { p.Opened[1].Key++ }},
		{"swapped indexes", func(p *Proof, _ *Day) { p.Opened[0].I, p.Opened[1].I = p.Opened[1].I, p.Opened[0].I }},
		{"duplicate index", func(p *Proof, _ *Day) { p.Edges[0].I = p.Opened[1].I }},
		{"index out of range", func(p *Proof, _ *Day) { p.Edges[0].I = p.Buckets }},
		{"extra hash", func(p *Proof, _ *Day) { p.Hashes = append(p.Hashes, Hash{}) }},
		{"missing hash", func(p *Proof, _ *Day) { p.Hashes = p.Hashes[:len(p.Hashes)-1] }},
		{"swapped hashes", func(p *Proof, _ *Day) { p.Hashes[0], p.Hashes[1] = p.Hashes[1], p.Hashes[0] }},
		{"wrong n", func(p *Proof, _ *Day) { p.Buckets++ }},
		{"zero n", func(p *Proof, d *Day) { p.Buckets, d.Buckets = 0, 0 }},
		{"other content hash", func(_ *Proof, d *Day) { d.ContentHash = strings.Repeat("ab", 32) }},
		{"seq above the block", func(_ *Proof, d *Day) { d.SeqTo = 11 }},
		{"seq below the block", func(_ *Proof, d *Day) { d.SeqFrom = 1 }},
		{"key outside the time span", func(_ *Proof, d *Day) { to := minuteOf(10); d.TimeTo = &to }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, rec := build(), d
			if len(p.Hashes) < 2 {
				t.Fatalf("fixture proof has %d hashes", len(p.Hashes))
			}
			tt.mutate(&p, &rec)
			if _, err := VerifyProof(p, rec); !errors.Is(err, ErrInvalidProof) {
				t.Fatalf("VerifyProof = %v, want ErrInvalidProof", err)
			}
		})
	}
}

// A block built wrongly (by a modified agent) verifies against its own
// root but is still refused: rows must be in their own bucket, in order,
// and minute buckets aren't empty.
func TestProofRejectsMisplacedRows(t *testing.T) {
	at := minuteOf(0)
	repo := RepoRow(1, at, Repo{Path: "/a", Project: "a"})
	act := func(seq int64, t time.Time) Row { return ActivityRow(seq, wire.ActivitySample{Time: t}) }
	line := func(r Row) []byte { l, _ := r.Line(); return l }
	tests := []struct {
		name  string
		repo  []Row
		other []Row
		key   uint64
	}{
		{"repo row in a minute bucket", nil, []Row{act(2, at), repo}, MinuteKey(at)},
		{"activity in the repo bucket", []Row{repo, act(2, at)}, []Row{act(3, at)}, MinuteKey(at)},
		{"row in another minute's bucket", []Row{repo}, []Row{act(2, at.Add(time.Minute))}, MinuteKey(at)},
		{"seq going back", []Row{repo}, []Row{act(3, at), act(2, at)}, MinuteKey(at)},
		{"empty minute bucket", []Row{repo}, nil, MinuteKey(at)},
		{"minute bucket keyed 0", []Row{repo}, []Row{act(2, at)}, RepoKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			salt := Hash{9}
			lines := func(rows []Row) ([][]byte, []string) {
				var b [][]byte
				var s []string
				for _, r := range rows {
					b, s = append(b, line(r)), append(s, string(line(r)))
				}
				return b, s
			}
			rb, rs := lines(tt.repo)
			ob, os := lines(tt.other)
			list := []Bucket{{Key: RepoKey, Salt: salt, Hash: bucketHash(salt, rb)}, {Key: tt.key, Salt: salt, Hash: bucketHash(salt, ob)}}
			leaves := []Hash{leafHash(list[0].Key, list[0].Hash), leafHash(list[1].Key, list[1].Hash)}
			from := at
			d := Day{ContentHash: rootOf(leaves).String(), Buckets: 2, SeqTo: 10, TimeFrom: &from, TimeTo: &from}
			p := Proof{Buckets: 2, Opened: []OpenedBucket{{I: 0, Key: RepoKey, Salt: salt, Lines: rs}, {I: 1, Key: tt.key, Salt: salt, Lines: os}}}
			if _, err := VerifyProof(p, d); !errors.Is(err, ErrInvalidProof) {
				t.Fatalf("VerifyProof = %v, want ErrInvalidProof", err)
			}
		})
	}
}

// BuildProof refuses to open a bucket whose rows no longer hash to it.
func TestBuildProofNeedsEveryRow(t *testing.T) {
	_, list, byIndex := sealed(t, fixtureBlock(), newSalts)
	short := map[int][]Row{1: byIndex[1][1:]}
	if _, err := BuildProof(list, short, nil); !errors.Is(err, ErrBucketIncomplete) {
		t.Fatalf("pruned bucket: %v", err)
	}
	wrong := map[int][]Row{1: byIndex[2]}
	if _, err := BuildProof(list, wrong, nil); !errors.Is(err, ErrBucketIncomplete) {
		t.Fatalf("another bucket's rows: %v", err)
	}
	if _, err := BuildProof(list, nil, []int{len(list)}); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("edge out of range: %v", err)
	}
	if _, err := BuildProof(list[1:], nil, nil); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("list without the repo bucket: %v", err)
	}
}

// Random blocks, random ranges: an honest proof verifies and proves every
// range complete, revealing exactly the block's rows in them; hiding one
// bucket of a range, or bounding it by the wrong edge, still verifies but
// isn't complete.
func TestProofProperties(t *testing.T) {
	for seed := range uint64(300) {
		checkProofProperties(t, seed)
	}
}

func FuzzProofProperties(f *testing.F) {
	for _, s := range []uint64{0, 1, 42, 1 << 40} {
		f.Add(s)
	}
	f.Fuzz(checkProofProperties)
}

func checkProofProperties(t *testing.T, seed uint64) {
	r := rand.New(rand.NewPCG(seed, 99))
	rows := randomRows(r, r.IntN(80))
	d, list, byIndex := sealed(t, rows, newSalts)
	start := time.Date(2026, 11, 19, 7, 50, 0, 0, time.UTC)
	var ranges []MinuteRange
	for range 1 + r.IntN(3) {
		from := start.Add(time.Duration(r.IntN(260)) * time.Minute)
		ranges = append(ranges, MinuteRange{From: from, To: from.Add(time.Duration(1+r.IntN(120)) * time.Minute)})
	}
	open, edges := PlanProof(list, ranges)
	p, err := BuildProof(list, opening(byIndex, open), edges)
	if err != nil {
		t.Fatalf("seed %d: %v", seed, err)
	}
	v, err := VerifyProof(p, d)
	if err != nil {
		t.Fatalf("seed %d: %v", seed, err)
	}
	revealed := map[int64]bool{}
	for _, row := range v.Rows() {
		revealed[row.Seq] = true
	}
	for _, rg := range ranges {
		if err := v.CheckComplete(rg); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for _, row := range rows {
			if row.Type != TypeRepo && !row.Time.Before(rg.From) && row.Time.Before(rg.To) && !revealed[row.Seq] {
				t.Fatalf("seed %d: seq %d in %v not revealed", seed, row.Seq, rg)
			}
		}
	}

	// Hide one opened minute bucket: the proof still verifies (it becomes
	// a subtree hash), but a range holding it is no longer complete.
	if len(open) > 0 {
		hidden := open[r.IntN(len(open))]
		fewer := opening(byIndex, slices.DeleteFunc(slices.Clone(open), func(i int) bool { return i == hidden }))
		p2, err := BuildProof(list, fewer, edges)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := VerifyProof(p2, d)
		if err != nil {
			t.Fatalf("seed %d: proof hiding a bucket: %v", seed, err)
		}
		if allComplete(v2, ranges) {
			t.Fatalf("seed %d: hiding bucket %d kept every range complete", seed, hidden)
		}
		// Revealing it only as an edge isn't enough either.
		p3, _ := BuildProof(list, fewer, append(slices.Clone(edges), hidden))
		if v3, err := VerifyProof(p3, d); err != nil || allComplete(v3, ranges) {
			t.Fatalf("seed %d: bucket %d as an edge: %v", seed, hidden, err)
		}
	}
	// Drop any edge: some range is no longer bounded.
	for k := range edges {
		p4, err := BuildProof(list, opening(byIndex, open), slices.Delete(slices.Clone(edges), k, k+1))
		if err != nil {
			t.Fatal(err)
		}
		v4, err := VerifyProof(p4, d)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if allComplete(v4, ranges) {
			t.Fatalf("seed %d: without edge %d every range is still complete", seed, edges[k])
		}
	}
}

func allComplete(v Verified, ranges []MinuteRange) bool {
	for _, rg := range ranges {
		if v.CheckComplete(rg) != nil {
			return false
		}
	}
	return true
}

// A range with no bucket in it is proven empty by the two leaves around
// it.
func TestCompletenessOfAnEmptyRange(t *testing.T) {
	d, list, byIndex := sealed(t, fixtureBlock(), newSalts)
	r := MinuteRange{From: minuteOf(5), To: minuteOf(20)}
	open, edges := PlanProof(list, []MinuteRange{r})
	if len(open) != 0 || len(edges) != 2 {
		t.Fatalf("open %v, edges %v", open, edges)
	}
	p, err := BuildProof(list, opening(byIndex, open), edges)
	if err != nil {
		t.Fatal(err)
	}
	v, err := VerifyProof(p, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.CheckComplete(r); err != nil {
		t.Fatal(err)
	}
	if err := v.CheckComplete(MinuteRange{From: minuteOf(0), To: minuteOf(20)}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a wider range: %v", err)
	}
	// Bounded one leaf further out, the leaf next to the range is hidden.
	wide, _ := BuildProof(list, nil, []int{edges[0] - 1, edges[1]})
	if v, err := VerifyProof(wide, d); err != nil || !errors.Is(v.CheckComplete(r), ErrIncomplete) {
		t.Fatalf("edge moved out: %v, %v", err, v.CheckComplete(r))
	}
	wide, _ = BuildProof(list, nil, []int{edges[0]})
	if v, err := VerifyProof(wide, d); err != nil || !errors.Is(v.CheckComplete(r), ErrIncomplete) {
		t.Fatalf("right edge dropped: %v, %v", err, v.CheckComplete(r))
	}
	// After the last leaf: one edge, and the range reaches the end.
	late := MinuteRange{From: minuteOf(40), To: minuteOf(50)}
	open, edges = PlanProof(list, []MinuteRange{late})
	p, _ = BuildProof(list, opening(byIndex, open), edges)
	if v, err = VerifyProof(p, d); err != nil || v.CheckComplete(late) != nil || len(edges) != 1 {
		t.Fatalf("range after the last leaf: %v, %v", err, v.CheckComplete(late))
	}
}

func TestNeedsCompleteness(t *testing.T) {
	from, to := minuteOf(0), minuteOf(30)
	d := Day{SeqFrom: 10, SeqTo: 20, TimeFrom: &from, TimeTo: &to}
	tests := []struct {
		name string
		d    Day
		r    MinuteRange
		h    int64
		want bool
	}{
		{"overlaps", d, MinuteRange{minuteOf(10), minuteOf(40)}, 15, true},
		{"touches the last minute", d, MinuteRange{minuteOf(30), minuteOf(31)}, 15, true},
		{"after the span", d, MinuteRange{minuteOf(31), minuteOf(40)}, 15, false},
		{"ends at the first minute", d, MinuteRange{minuteOf(-10), minuteOf(0)}, 15, false},
		{"block starts at H", d, MinuteRange{minuteOf(10), minuteOf(40)}, 10, false},
		{"no time span", Day{SeqFrom: 0, SeqTo: 5}, MinuteRange{minuteOf(10), minuteOf(40)}, 15, false},
	}
	for _, tt := range tests {
		if got := NeedsCompleteness(tt.d, tt.r, tt.h); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}
