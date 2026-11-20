package rollup

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

var (
	keyA = Key{Project: "alpha", Language: "Go", Editor: "vscode", Category: "coding"}
	keyB = Key{Project: "beta", Language: "Rust", Editor: "neovim", Category: "debugging"}
)

func hb(d time.Duration, k Key) Heartbeat { return Heartbeat{Time: at(d), Key: k} }

// everything is one window wide enough to hold every test's heartbeats.
var everything = []Range{{Start: t0.Add(-time.Hour), End: t0.Add(48 * time.Hour)}}

func TestCompute(t *testing.T) {
	s := time.Second
	tests := []struct {
		name string
		hbs  []Heartbeat
		want []Row
	}{
		{
			name: "single heartbeat credits nothing",
			hbs:  []Heartbeat{hb(0, keyA)},
			want: nil,
		},
		{
			name: "gap is credited to the earlier heartbeat",
			hbs:  []Heartbeat{hb(0, keyA), hb(30*s, keyB)},
			want: []Row{{Minute: at(0), Key: keyA, Seconds: 30}},
		},
		{
			name: "gap of exactly the timeout counts and splits across minutes",
			hbs:  []Heartbeat{hb(0, keyA), hb(2*time.Minute, keyA)},
			want: []Row{{Minute: at(0), Key: keyA, Seconds: 60}, {Minute: at(time.Minute), Key: keyA, Seconds: 60}},
		},
		{
			name: "gap over the timeout credits nothing",
			hbs:  []Heartbeat{hb(0, keyA), hb(2*time.Minute+s, keyA)},
			want: nil,
		},
		{
			name: "span is split at the minute boundary",
			hbs:  []Heartbeat{hb(50*s, keyA), hb(80*s, keyA)},
			want: []Row{{Minute: at(0), Key: keyA, Seconds: 10}, {Minute: at(time.Minute), Key: keyA, Seconds: 20}},
		},
		{
			name: "same time heartbeats credit zero to the first",
			hbs:  []Heartbeat{hb(0, keyB), hb(0, keyA), hb(20*s, keyA)},
			want: []Row{{Minute: at(0), Key: keyA, Seconds: 20}},
		},
		{
			name: "empty project rolls up as Unknown",
			hbs:  []Heartbeat{hb(0, Key{Language: "Go"}), hb(10*s, keyA)},
			want: []Row{{Minute: at(0), Key: Key{Project: UnknownProject, Language: "Go"}, Seconds: 10}},
		},
		{
			name: "groups in one minute share it and sum to the wall clock",
			hbs:  []Heartbeat{hb(0, keyA), hb(20*s, keyB), hb(40*s, keyA), hb(60*s, keyB)},
			want: []Row{{Minute: at(0), Key: keyA, Seconds: 40}, {Minute: at(0), Key: keyB, Seconds: 20}},
		},
		{
			name: "fractional seconds round with largest remainder",
			hbs: []Heartbeat{
				hb(0, keyA), hb(20500*time.Millisecond, keyB), hb(41*s, keyA), hb(60*s, keyB),
			},
			// keyA: 20.5 + 19 = 39.5, keyB: 20.5; total 60.
			want: []Row{{Minute: at(0), Key: keyA, Seconds: 40}, {Minute: at(0), Key: keyB, Seconds: 20}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compute(tt.hbs, everything)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Compute() =\n%v\nwant\n%v", got, tt.want)
			}
			assertCap(t, got)
		})
	}
}

func TestComputeClipsToWindows(t *testing.T) {
	hbs := []Heartbeat{hb(0, keyA), hb(90*time.Second, keyA), hb(150*time.Second, keyA)}
	windows := []Range{{Start: at(time.Minute), End: at(2 * time.Minute)}}
	got := Compute(hbs, windows)
	want := []Row{{Minute: at(time.Minute), Key: keyA, Seconds: 60}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Compute() = %v, want %v", got, want)
	}
}

func TestRoundMinuteScalesOverlapsToSixty(t *testing.T) {
	tests := []struct {
		name   string
		groups map[Key]time.Duration
		want   map[Key]int16
	}{
		{
			name:   "two machines claiming 45s each scale to 30s each",
			groups: map[Key]time.Duration{keyA: 45 * time.Second, keyB: 45 * time.Second},
			want:   map[Key]int16{keyA: 30, keyB: 30},
		},
		{
			name:   "proportions are kept",
			groups: map[Key]time.Duration{keyA: 90 * time.Second, keyB: 30 * time.Second},
			want:   map[Key]int16{keyA: 45, keyB: 15},
		},
		{
			name: "rounding never pushes a full minute over sixty",
			groups: map[Key]time.Duration{
				keyA: 30500 * time.Millisecond, keyB: 29500 * time.Millisecond,
			},
			want: map[Key]int16{keyA: 31, keyB: 29},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := roundMinute(t0, tt.groups)
			got := map[Key]int16{}
			for _, r := range rows {
				got[r.Key] = r.Seconds
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("roundMinute() = %v, want %v", got, tt.want)
			}
			assertCap(t, rows)
		})
	}
}

func TestWindows(t *testing.T) {
	m := time.Minute
	tests := []struct {
		name  string
		times []time.Time
		want  []Range
	}{
		{name: "none", times: nil, want: nil},
		{
			name:  "one heartbeat covers two minutes either side, whole minutes",
			times: []time.Time{at(30 * time.Second)},
			want:  []Range{{Start: at(-2 * m), End: at(3 * m)}},
		},
		{
			name:  "close heartbeats merge regardless of order",
			times: []time.Time{at(6 * m), at(0)},
			want:  []Range{{Start: at(-2 * m), End: at(9 * m)}},
		},
		{
			name:  "distant heartbeats stay separate",
			times: []time.Time{at(0), at(24 * time.Hour)},
			want: []Range{
				{Start: at(-2 * m), End: at(3 * m)},
				{Start: at(24*time.Hour - 2*m), End: at(24*time.Hour + 3*m)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Windows(tt.times)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Windows() = %v, want %v", got, tt.want)
			}
			for i := 1; i < len(got); i++ {
				_, prevTo := ReadRange(got[i-1])
				from, _ := ReadRange(got[i])
				if !from.After(prevTo) {
					t.Fatalf("read ranges %d and %d overlap", i-1, i)
				}
			}
		})
	}
}

// TestIncrementalMatchesFullRecompute simulates the ingest loop: heartbeats
// arrive in random batches and order, each batch recomputes only its
// windows, and the final table must equal a recompute from scratch.
func TestIncrementalMatchesFullRecompute(t *testing.T) {
	for seed := range uint64(50) {
		rng := rand.New(rand.NewPCG(seed, 7))
		all := randomHeartbeats(rng, 200)
		want := Compute(sorted(all), everything)

		shuffled := slices.Clone(all)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		table := map[time.Time][]Row{}
		var stored []Heartbeat
		for len(shuffled) > 0 {
			n := min(len(shuffled), 1+rng.IntN(30))
			batch := shuffled[:n]
			shuffled = shuffled[n:]
			stored = append(stored, batch...)

			times := make([]time.Time, len(batch))
			for i, h := range batch {
				times[i] = h.Time
			}
			windows := Windows(times)
			for m := range table {
				if inWindows(windows, m) {
					delete(table, m)
				}
			}
			for _, r := range Compute(readable(sorted(stored), windows), windows) {
				table[r.Minute] = append(table[r.Minute], r)
			}
		}

		var got []Row
		for _, rows := range table {
			got = append(got, rows...)
		}
		slices.SortFunc(got, func(a, b Row) int {
			if c := a.Minute.Compare(b.Minute); c != 0 {
				return c
			}
			return a.Key.compare(b.Key)
		})
		if !reflect.DeepEqual(got, want) {
			gm, wm := map[Row]bool{}, map[Row]bool{}
			for _, r := range got {
				gm[r] = true
			}
			for _, r := range want {
				wm[r] = true
			}
			for r := range gm {
				if !wm[r] {
					t.Logf("extra %v", r)
				}
			}
			for r := range wm {
				if !gm[r] {
					t.Logf("missing %v", r)
				}
			}
			t.Fatalf("seed %d: incremental rollup differs from full recompute", seed)
		}
		assertCap(t, got)
	}
}

// readable keeps only the heartbeats inside the windows' read ranges, as the
// database query does, so the test proves the ranges are wide enough.
func readable(hbs []Heartbeat, windows []Range) []Heartbeat {
	var out []Heartbeat
	for _, h := range hbs {
		for _, w := range windows {
			from, to := ReadRange(w)
			if !h.Time.Before(from) && !h.Time.After(to) {
				out = append(out, h)
				break
			}
		}
	}
	return out
}

func randomHeartbeats(rng *rand.Rand, n int) []Heartbeat {
	keys := []Key{keyA, keyB, {Project: "", Language: "Python", Editor: "vim", Category: "coding"}}
	out := make([]Heartbeat, n)
	cur := t0
	for i := range out {
		// Mostly short gaps, sometimes a break longer than the timeout.
		gap := time.Duration(rng.IntN(90_000)) * time.Millisecond
		if rng.IntN(10) == 0 {
			gap = time.Duration(rng.IntN(600)) * time.Second
		}
		cur = cur.Add(gap)
		out[i] = Heartbeat{Time: cur, Key: keys[rng.IntN(len(keys))]}
	}
	return out
}

func sorted(hbs []Heartbeat) []Heartbeat {
	out := slices.Clone(hbs)
	// Ties break on the key, standing in for the fields_hash order the database uses.
	slices.SortFunc(out, func(a, b Heartbeat) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return a.Key.compare(b.Key)
	})
	return out
}

func assertCap(t *testing.T, rows []Row) {
	t.Helper()
	perMinute := map[time.Time]int{}
	for _, r := range rows {
		if r.Seconds <= 0 {
			t.Fatalf("row with %d seconds: %+v", r.Seconds, r)
		}
		perMinute[r.Minute] += int(r.Seconds)
	}
	for m, s := range perMinute {
		if s > 60 {
			t.Fatalf("minute %v holds %d seconds", m, s)
		}
	}
}
