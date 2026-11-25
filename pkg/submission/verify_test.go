package submission

import (
	"crypto/ed25519"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
)

func results(t *testing.T, c Checked) map[Result]int {
	t.Helper()
	out := map[Result]int{}
	for _, r := range c.Results {
		out[r.Result]++
	}
	return out
}

// An honest part verifies every frozen row, from both blocks, and the
// fixture is worth checking: it has credited, split, agent and idle rows.
func TestVerifyHonestPart(t *testing.T) {
	f := newFixture(t)
	if l := f.labels(); l["you"] < 30 || l["agent"] < 5 || l["idle"] < 1 {
		t.Fatalf("fixture labels = %v", l)
	}
	split := slices.ContainsFunc(f.frozen, func(r Minute) bool { return r.Credited == 30 })
	if !split || len(f.lib) == 0 {
		t.Fatalf("fixture has no minute split with lib")
	}
	p := f.part(t, partOpts{})
	if len(p.Blocks) != 2 {
		t.Fatalf("part has %d blocks, want both", len(p.Blocks))
	}
	c, err := VerifyPart(p, f.dev, f.frozen)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Results {
		if r.Result != ResultVerified {
			t.Fatalf("%s: %s %s", r.Row.Minute.Format(time.RFC3339), r.Result, r.Reason)
		}
	}
	if !slices.IsSortedFunc(c.Revealed, func(a, b evidence.Row) int { return int(a.Seq - b.Seq) }) || len(c.Revealed) == 0 {
		t.Fatal("revealed rows are not sorted by seq")
	}
	// The run's whole window was opened, back to 22:40 of the day before.
	if !slices.ContainsFunc(c.Revealed, func(r evidence.Row) bool { return r.Time.Before(at(-1, 30)) && r.Type == evidence.TypeActivity }) {
		t.Fatal("rows of the day before weren't revealed")
	}
	// The order rows come in doesn't matter.
	rev := slices.Clone(f.frozen)
	slices.Reverse(rev)
	dev := f.dev
	dev.Records = slices.Clone(dev.Records)
	slices.Reverse(dev.Records)
	c2, err := VerifyPart(p, dev, rev)
	if err != nil || results(t, c2)[ResultVerified] != len(f.frozen) {
		t.Fatalf("reversed input: %v %v", err, results(t, c2))
	}
}

// Every tamper with the evidence itself fails the part: the submission is
// failed, not shown as unbacked.
func TestVerifyRejectsTamperedEvidence(t *testing.T) {
	f := newFixture(t)
	honest := f.part(t, partOpts{})
	clone := func() Part {
		p := honest
		p.Blocks = slices.Clone(p.Blocks)
		for i := range p.Blocks {
			b := &p.Blocks[i]
			b.Opened = slices.Clone(b.Opened)
			for j := range b.Opened {
				b.Opened[j].Lines = slices.Clone(b.Opened[j].Lines)
			}
			b.Edges = slices.Clone(b.Edges)
			b.Hashes = slices.Clone(b.Hashes)
		}
		p.Minutes = slices.Clone(p.Minutes)
		return p
	}
	// The runs at T = 00:40 read up to 00:40, so block B's proof for them
	// alone ends with the leaf of 00:40 as an edge.
	early := func() []Minute {
		return filter(f.frozen, func(r Minute) bool { k, _ := r.Key(); return k.T.Equal(at(0, 40)) })
	}
	if p := f.part(t, partOpts{rows: early()}); len(p.Blocks) != 2 || len(p.Blocks[1].Edges) != 1 {
		t.Fatalf("the part for T = 00:40 has no edge in block B: %+v", p.Blocks)
	}
	other := ed25519.NewKeyFromSeed(append(make([]byte, ed25519.SeedSize-1), 1))
	tests := []struct {
		name  string
		part  func() Part
		dev   func() Device
		rows  func() []Minute
		chain bool // ErrBadChain
	}{
		{name: "an opened row edited", part: func() Part {
			p := clone()
			o := &p.Blocks[0].Opened[3]
			o.Lines[0] = strings.Replace(o.Lines[0], `"idle_seconds":1`, `"idle_seconds":0`, 1)
			return p
		}},
		{name: "a row removed from an opened bucket", part: func() Part {
			p := clone()
			o := &p.Blocks[0].Opened[3]
			o.Lines = o.Lines[1:]
			return p
		}},
		{name: "a row added to an opened bucket", part: func() Part {
			p := clone()
			o := &p.Blocks[1].Opened[2]
			o.Lines = append(o.Lines, o.Lines[len(o.Lines)-1])
			return p
		}},
		{name: "an edge's key moved", rows: early, part: func() Part {
			p := f.part(t, partOpts{rows: early()})
			p.Blocks[1].Edges[0].Key++
			return p
		}},
		{name: "an edge's hash changed", rows: early, part: func() Part {
			p := f.part(t, partOpts{rows: early()})
			p.Blocks[1].Edges[0].BucketHash[0] ^= 1
			return p
		}},
		{name: "a subtree hash dropped", rows: early, part: func() Part {
			p := f.part(t, partOpts{rows: early()})
			p.Blocks[1].Hashes = p.Blocks[1].Hashes[1:]
			return p
		}},
		{name: "a hidden bucket's hash replaced", rows: early, part: func() Part {
			p := f.part(t, partOpts{rows: early()})
			p.Blocks[1].Hashes[0][0] ^= 1
			return p
		}},
		{name: "a block shown as another day", part: func() Part {
			p := clone()
			p.Blocks[0].Day, p.Blocks[1].Day = p.Blocks[1].Day, p.Blocks[0].Day
			return p
		}},
		{name: "a block with no stored day", part: func() Part {
			p := clone()
			p.Blocks[1].Day = "2026-11-21"
			return p
		}},
		{name: "the same day twice", part: func() Part {
			p := clone()
			p.Blocks = append(p.Blocks, p.Blocks[0])
			return p
		}},
		{name: "a minute the server didn't freeze", part: func() Part {
			p := clone()
			p.Minutes = append(p.Minutes, PartMinute{Minute: at(2, 0), ComputedAt: at(2, 30), Snapshot: 5})
			return p
		}},
		{name: "a minute with another key", part: func() Part {
			p := clone()
			p.Minutes[0].Snapshot--
			return p
		}},
		{name: "a part of another device", part: func() Part {
			p := clone()
			p.DeviceID = devB
			return p
		}},
		{name: "a frozen row of another device", rows: func() []Minute {
			rows := slices.Clone(f.frozen)
			rows[0].DeviceID = devB
			return rows
		}},
		{name: "a rules version this build can't run", rows: func() []Minute {
			rows := slices.Clone(f.frozen)
			rows[0].RulesVersion = "v0"
			return rows
		}},
		{name: "a record's content hash changed", chain: true, dev: func() Device {
			d := f.dev
			d.Records = slices.Clone(d.Records)
			d.Records[1].ContentHash = strings.Repeat("0", 64)
			return d
		}},
		{name: "a record's seq bounds changed", chain: true, dev: func() Device {
			d := f.dev
			d.Records = slices.Clone(d.Records)
			d.Records[1].SeqTo++
			return d
		}},
		{name: "another device's key", chain: true, dev: func() Device {
			d := f.dev
			d.PublicKey = other.Public().(ed25519.PublicKey)
			return d
		}},
		{name: "a signature flipped", chain: true, dev: func() Device {
			d := f.dev
			d.Records = slices.Clone(d.Records)
			d.Records[0].Signature = slices.Clone(d.Records[0].Signature)
			d.Records[0].Signature[5] ^= 1
			return d
		}},
		{name: "a middle record left out", chain: true, dev: func() Device {
			// A third day on top of the fixture's two, then the second
			// left out: the third doesn't follow the first.
			d := f.dev
			third := evidence.Day{DeviceID: devA, Day: "2026-11-21", PrevChainHash: d.Records[1].ChainHash,
				ContentHash: strings.Repeat("ab", 32), RulesVersion: d.Records[1].RulesVersion,
				SeqFrom: d.Records[1].SeqTo, SeqTo: d.Records[1].SeqTo + 1, Buckets: 1}
			chain, sig, err := evidence.Sign(f.priv, third)
			if err != nil {
				t.Fatal(err)
			}
			d.Records = []EvidenceRecord{d.Records[0], {Day: third.Day, PrevChainHash: third.PrevChainHash,
				ContentHash: third.ContentHash, ChainHash: chain, Signature: sig, RulesVersion: third.RulesVersion,
				SeqFrom: third.SeqFrom, SeqTo: third.SeqTo, Buckets: 1}}
			return d
		}},
		{name: "a record signed by another key", chain: true, dev: func() Device {
			d := f.dev
			d.Records = slices.Clone(d.Records)
			r := &d.Records[1]
			sd := r.Signed(devA)
			_, sig, err := evidence.Sign(other, sd)
			if err != nil {
				t.Fatal(err)
			}
			r.Signature = sig
			return d
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, dev, rows := honest, f.dev, f.frozen
			if tt.part != nil {
				p = tt.part()
			}
			if tt.dev != nil {
				dev = tt.dev()
			}
			if tt.rows != nil {
				rows = tt.rows()
			}
			_, err := VerifyPart(p, dev, rows)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("VerifyPart = %v, want ErrMalformed", err)
			}
			if tt.chain && !errors.Is(err, ErrBadChain) {
				t.Fatalf("VerifyPart = %v, want ErrBadChain", err)
			}
		})
	}
}

// A part that doesn't show everything a run read leaves that run's
// minutes unbacked, with the reason, and every other minute verified.
func TestVerifyUnbacked(t *testing.T) {
	f := newFixture(t)
	key := func(r Minute) replay.Key { k, _ := r.Key(); return k }
	t0040 := func(r Minute) bool { return key(r).T.Equal(at(0, 40)) }
	tests := []struct {
		name string
		part func() Part
		dev  func() Device
		rows func() []Minute
		// want is the reason for rows it holds, verified for the rest.
		want   string
		which  func(Minute) bool
		listed func(Minute) bool // rows the part lists (all when nil)
	}{
		{
			// 23:25 is in every window but the one of T = 01:30.
			name: "a minute bucket of the window hidden", want: ReasonIncomplete,
			part:  func() Part { return f.part(t, partOpts{hide: []uint64{evidence.MinuteKey(at(-1, 25))}}) },
			which: func(r Minute) bool { return !key(r).T.Equal(at(1, 30)) },
		},
		{
			// The late sample of 00:10 is in block B. Runs before it was
			// logged didn't read it, but the server can't tell from the
			// signed fields alone (B starts below their H and its span
			// overlaps their window), so every run needs it opened.
			name: "a late row's bucket hidden in the next block", want: ReasonIncomplete,
			part:  func() Part { return f.part(t, partOpts{hide: []uint64{evidence.MinuteKey(at(0, 10))}}) },
			which: func(Minute) bool { return true },
		},
		{
			// Backing only T = 00:40, block B's window ends at 00:40, so
			// the leaf of 00:40 is its right edge.
			name: "the edge after the window left out", want: ReasonIncomplete,
			listed: t0040,
			part: func() Part {
				return f.part(t, partOpts{rows: filter(f.frozen, t0040), dropEdges: []uint64{evidence.MinuteKey(at(0, 40))}})
			},
			which: t0040,
		},
		{
			name: "the repo bucket of H's block not opened", want: ReasonNoRepo,
			part:  func() Part { return f.part(t, partOpts{noRepo: true}) },
			which: func(Minute) bool { return true },
		},
		{
			// Block A's span runs to 00:29, so every window overlaps it.
			name: "the block before left out", want: ReasonIncomplete,
			part:  func() Part { return f.part(t, partOpts{skipDays: []string{"2026-11-19"}}) },
			which: func(Minute) bool { return true },
		},
		{
			name: "H's block left out", want: ReasonNoRepo,
			part:  func() Part { return f.part(t, partOpts{skipDays: []string{"2026-11-20"}}) },
			which: func(Minute) bool { return true },
		},
		{
			// A minute claiming H in a block that was never sealed.
			name: "H's block not sealed", want: ReasonNotSealed,
			part: func() Part { return f.part(t, partOpts{skipDays: []string{"2026-11-20"}}) },
			dev: func() Device {
				d := f.dev
				d.Records = d.Records[:1]
				return d
			},
			which: func(Minute) bool { return true },
		},
		{
			// Without the first record, the chain starts at block B,
			// which links to an earlier record and starts above 0: rows
			// of block A that the runs read are in no record given.
			name: "the chain's first record left out", want: ReasonChainStart,
			part: func() Part { return f.part(t, partOpts{skipDays: []string{"2026-11-19"}}) },
			dev: func() Device {
				d := f.dev
				d.Records = d.Records[1:]
				return d
			},
			which: func(Minute) bool { return true },
		},
		{
			name: "rows the part doesn't list", want: ReasonNotListed,
			listed: func(r Minute) bool { return !t0040(r) },
			part: func() Part {
				return f.part(t, partOpts{rows: filter(f.frozen, func(r Minute) bool { return !t0040(r) })})
			},
			which: t0040,
		},
		{
			name: "a row stored before computation keys", want: ReasonNoKey,
			rows: func() []Minute {
				rows := slices.Clone(f.frozen)
				rows[0].ComputedAt, rows[0].Snapshot = nil, nil
				return rows
			},
			listed: func(r Minute) bool { return !r.Minute.Equal(f.frozen[0].Minute) },
			part: func() Part {
				return f.part(t, partOpts{rows: filter(f.frozen, func(r Minute) bool { return !r.Minute.Equal(f.frozen[0].Minute) })})
			},
			which: func(r Minute) bool { return r.Minute.Equal(f.frozen[0].Minute) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev, rows := f.dev, f.frozen
			if tt.dev != nil {
				dev = tt.dev()
			}
			if tt.rows != nil {
				rows = tt.rows()
			}
			c, err := VerifyPart(tt.part(), dev, rows)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, r := range c.Results {
				want, reason := ResultVerified, ""
				switch {
				case tt.which(r.Row):
					want, reason = ResultUnbacked, tt.want
					n++
				case tt.listed != nil && !tt.listed(r.Row):
					want, reason = ResultUnbacked, ReasonNotListed
				}
				if r.Result != want || r.Reason != reason {
					t.Fatalf("%s at %v: %s %q, want %s %q", r.Row.Minute.Format(time.RFC3339), key(r.Row).T.Format("15:04"),
						r.Result, r.Reason, want, reason)
				}
			}
			if n == 0 {
				t.Fatal("no row is unbacked: the case tests nothing")
			}
		})
	}
}

func filter(rows []Minute, keep func(Minute) bool) []Minute {
	var out []Minute
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// A frozen row the replay doesn't reproduce is a mismatch, with the
// server's row next to it; flags are advisory and never mismatch.
func TestVerifyMismatch(t *testing.T) {
	f := newFixture(t)
	p := f.part(t, partOpts{})
	find := func(label string, credited int) int {
		i := slices.IndexFunc(f.frozen, func(r Minute) bool { return r.Label == label && r.Credited == credited })
		if i < 0 {
			t.Fatalf("no %s row with %d s", label, credited)
		}
		return i
	}
	you, agent := find("you", 60), find("agent", 0)
	tests := []struct {
		name   string
		i      int
		edit   func(*Minute)
		result Result
	}{
		{"credit raised", find("you", 30), func(m *Minute) { m.Credited = 60 }, ResultMismatch},
		{"credit lowered", you, func(m *Minute) { m.Credited = 45 }, ResultMismatch},
		{"agent minute claimed as the person's", agent, func(m *Minute) { m.Label, m.Credited, m.Cuts = "you", 60, []string{} }, ResultMismatch},
		{"a cut added", you, func(m *Minute) { m.Credited, m.Cuts = 0, []string{"loop"} }, ResultMismatch},
		{"a cut dropped", agent, func(m *Minute) { m.Cuts = []string{} }, ResultMismatch},
		{"a flag added", you, func(m *Minute) { m.Flags = []string{"scripted_input"} }, ResultVerified},
		{"cuts in another order", agent, func(m *Minute) { m.Cuts = slices.Clone(m.Cuts); slices.Reverse(m.Cuts) }, ResultVerified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := slices.Clone(f.frozen)
			orig := rows[tt.i]
			tt.edit(&rows[tt.i])
			c, err := VerifyPart(p, f.dev, rows)
			if err != nil {
				t.Fatal(err)
			}
			for i, r := range c.Results {
				want := ResultVerified
				if i == tt.i {
					want = tt.result
				}
				if r.Result != want {
					t.Fatalf("row %d (%s): %s, want %s", i, r.Row.Minute.Format(time.RFC3339), r.Result, want)
				}
			}
			got := c.Results[tt.i]
			if tt.result == ResultMismatch {
				s := got.Server
				if s == nil || s.Label != orig.Label || s.Credited != orig.Credited || !slices.Equal(s.Cuts, orig.Cuts) {
					t.Fatalf("server = %+v, want the replay's %+v", s, orig)
				}
			} else if got.Server != nil {
				t.Fatalf("server = %+v on a verified row", got.Server)
			}
		})
	}
	// A row for a minute in which the replay has no row of the project:
	// the server's side is idle with nothing credited.
	t.Run("a minute the replay doesn't credit at all", func(t *testing.T) {
		var ref Minute
		for _, r := range f.frozen {
			if k, _ := r.Key(); k.T.Equal(at(1, 30)) {
				ref = r
			}
		}
		extra := ref
		extra.Minute, extra.Label, extra.Credited, extra.Cuts = at(0, 59), "you", 60, []string{}
		if ref.ComputedAt == nil || slices.ContainsFunc(f.frozen, func(r Minute) bool { return r.Minute.Equal(extra.Minute) }) {
			t.Fatal("the fixture has a row at 00:59 or none computed at 01:30")
		}
		rows := append(slices.Clone(f.frozen), extra)
		c, err := VerifyPart(f.part(t, partOpts{rows: rows}), f.dev, rows)
		if err != nil {
			t.Fatal(err)
		}
		got := c.Results[len(rows)-1]
		if got.Result != ResultMismatch || got.Server == nil || got.Server.Label != "idle" || got.Server.Credited != 0 || len(got.Server.Cuts) != 0 {
			t.Fatalf("result = %+v", got)
		}
	})
}

func TestPartValidate(t *testing.T) {
	good := Part{DeviceID: devA, Minutes: []PartMinute{{Minute: at(0, 10), ComputedAt: at(0, 40), Snapshot: 3}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(p *Part){
		"device id not a uuid":       func(p *Part) { p.DeviceID = "laptop" },
		"device id in upper case":    func(p *Part) { p.DeviceID = strings.ToUpper(devA) },
		"minute after its key":       func(p *Part) { p.Minutes[0].Minute = at(0, 40) },
		"minute before its window":   func(p *Part) { p.Minutes[0].Minute = at(-1, 39) },
		"minute not whole":           func(p *Part) { p.Minutes[0].Minute = at(0, 10).Add(time.Second) },
		"key not whole":              func(p *Part) { p.Minutes[0].ComputedAt = at(0, 40).Add(time.Second) },
		"negative snapshot":          func(p *Part) { p.Minutes[0].Snapshot = -1 },
		"a minute twice":             func(p *Part) { p.Minutes = append(p.Minutes, p.Minutes[0]) },
		"a block day that isn't one": func(p *Part) { p.Blocks = []Block{{Day: "20-11-2026", Proof: evidence.Proof{Buckets: 1}}} },
		"two blocks for a day": func(p *Part) {
			p.Blocks = []Block{{Day: "2026-11-20", Proof: evidence.Proof{Buckets: 1}}, {Day: "2026-11-20", Proof: evidence.Proof{Buckets: 1}}}
		},
		"a block with no buckets": func(p *Part) { p.Blocks = []Block{{Day: "2026-11-20"}} },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			p := good
			p.Minutes = slices.Clone(good.Minutes)
			edit(&p)
			if err := p.Validate(); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Validate = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestParseRange(t *testing.T) {
	r, err := ParseRange("2026-11-01", "2026-11-03")
	if err != nil {
		t.Fatal(err)
	}
	if d := r.Days(); !slices.Equal(d, []string{"2026-11-01", "2026-11-02", "2026-11-03"}) {
		t.Fatalf("days = %v", d)
	}
	if !r.Contains(time.Date(2026, 11, 3, 23, 59, 0, 0, time.UTC)) || r.Contains(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("Contains is wrong at the end of the range")
	}
	if _, err := ParseRange("2026-11-01", "2027-01-31"); err != nil {
		t.Fatalf("92 days: %v", err)
	}
	for _, bad := range [][2]string{{"2026-11-01", "2027-02-01"}, {"2026-11-02", "2026-11-01"}, {"2026-11-1", "2026-11-02"}, {"", ""}} {
		if _, err := ParseRange(bad[0], bad[1]); !errors.Is(err, ErrMalformed) {
			t.Errorf("ParseRange(%q, %q) = %v", bad[0], bad[1], err)
		}
	}
}
