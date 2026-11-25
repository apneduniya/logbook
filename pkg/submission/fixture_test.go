package submission

import (
	"crypto/ed25519"
	"slices"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

const (
	devA = "a0000000-0000-4000-8000-000000000001"
	devB = "b0000000-0000-4000-8000-000000000002"
)

// midnight is the start of the submitted day, 2026-11-20.
var midnight = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

// at is midnight plus h hours and m minutes (h may be -1 for the day
// before).
func at(h, m int) time.Time {
	return midnight.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

// counter is a deterministic salt source: every byte is the count of
// bytes read before it, mod 251.
type counter struct{ n int }

func (c *counter) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c.n % 251)
		c.n++
	}
	return len(p), nil
}

// logged is a row with the time the agent logged it, which decides its
// sequence number.
type logged struct {
	at  time.Time
	row func(seq int64) evidence.Row
}

// fxBlock is one sealed block of the fixture.
type fxBlock struct {
	rec   EvidenceRecord
	list  []evidence.Bucket
	byKey map[uint64][]evidence.Row
}

// fixture is device A's log, its two sealed blocks, the minutes it
// computed on 2026-11-20 and what the server froze for project app.
//
// The person works in app from 23:20 (the day before) to 00:50, with a
// heartbeat each minute and activity every 15 s. 00:05 to 00:07 also have
// a heartbeat in lib (another project, so those minutes split 30/30). At
// 00:24 they prompt Claude Code and walk away from 00:25 to 00:38 while
// it works (agent minutes), and commit at 00:20. An activity sample of
// 00:10 is logged late, at 00:45, so it is in the second block. Block A
// (day 2026-11-19) holds every row logged before 00:30; block B (day
// 2026-11-20) opens with a repo snapshot and holds the rest.
//
// Minute m is computed at T = m rounded down to 10 minutes + 40 minutes,
// with H the last row logged before T, so each key covers 10 minutes.
type fixture struct {
	priv   ed25519.PrivateKey
	dev    Device
	rows   []evidence.Row
	blocks []fxBlock
	frozen []Minute
	// lib are A's credited rows of other projects, for cap inputs.
	lib []Minute
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	var evs []logged
	add := func(when time.Time, row func(seq int64) evidence.Row) { evs = append(evs, logged{at: when, row: row}) }
	repos := func(when time.Time) {
		add(when, func(s int64) evidence.Row {
			return evidence.RepoRow(s, when, evidence.Repo{Path: "/src/app", Project: "app"})
		})
		add(when, func(s int64) evidence.Row {
			return evidence.RepoRow(s, when, evidence.Repo{Path: "/src/lib", Project: "lib"})
		})
	}
	repos(at(-2, 0))
	away := func(t time.Time) bool { return !t.Before(at(0, 25)) && t.Before(at(0, 38)) }
	for s := at(-1, 20); s.Before(at(0, 50)); s = s.Add(15 * time.Second) {
		if away(s) {
			continue
		}
		add(s, func(seq int64) evidence.Row {
			return evidence.ActivityRow(seq, wire.ActivitySample{Time: s, IdleSeconds: 1, FocusApp: "Code", FocusKind: "editor"})
		})
		if s.Second() == 0 {
			add(s, func(seq int64) evidence.Row {
				return evidence.HeartbeatRow(seq, wire.Heartbeat{Entity: "/src/app/main.go", Time: float64(s.Unix()) + 0.5,
					Project: "app", IsWrite: s.Minute()%3 == 0, Category: "coding"})
			})
		}
		if s.Second() == 30 && !s.Before(at(0, 5)) && s.Before(at(0, 8)) {
			add(s, func(seq int64) evidence.Row {
				return evidence.HeartbeatRow(seq, wire.Heartbeat{Entity: "/src/lib/x.go", Time: float64(s.Unix()), Project: "lib", IsWrite: true})
			})
		}
	}
	ai := func(when time.Time, kind wire.AIEventKind) {
		add(when, func(seq int64) evidence.Row {
			e := wire.AIEvent{Time: when, Harness: wire.HarnessClaudeCode, Kind: kind, SessionID: "s1", Cwd: "/src/app", Model: "claude-x"}
			if kind == wire.AIPromptSubmit {
				e.PromptChars, e.PromptHash = 80, "deadbeef"
			}
			return evidence.AIRow(seq, e)
		})
	}
	ai(at(0, 24), wire.AIPromptSubmit)
	ai(at(0, 27), wire.AIToolStart)
	ai(at(0, 33), wire.AIToolEnd)
	ai(at(0, 37), wire.AITurnEnd)
	add(at(0, 20), func(seq int64) evidence.Row {
		return evidence.GitRow(seq, wire.GitEvent{Time: at(0, 20), Project: "app", Kind: "commit",
			CommitHash: "0123456789abcdef0123456789abcdef01234567", FilesChanged: 2, Insertions: 30, Deletions: 4})
	})
	add(at(0, 31), func(seq int64) evidence.Row {
		return evidence.ActiveTimeRow(seq, evidence.ActiveTime{From: at(0, 30), To: at(0, 31), SessionID: "s1", CLISecs: 50,
			Harness: wire.HarnessClaudeCode})
	})
	late := at(0, 10).Add(5 * time.Second)
	add(at(0, 45), func(seq int64) evidence.Row {
		return evidence.ActivityRow(seq, wire.ActivitySample{Time: late, IdleSeconds: 2, FocusApp: "Code", FocusKind: "editor"})
	})
	// Sealing day 2026-11-19 at 00:30 logs the snapshot that opens block B.
	repos(at(0, 30))
	slices.SortStableFunc(evs, func(a, b logged) int { return a.at.Compare(b.at) })

	f := &fixture{}
	f.priv = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	f.dev = Device{ID: devA, PublicKey: f.priv.Public().(ed25519.PublicKey)}
	var seqTo int64 // the last row logged before 00:30
	logTime := map[int64]time.Time{}
	for i, e := range evs {
		seq := int64(i + 1)
		f.rows = append(f.rows, e.row(seq))
		logTime[seq] = e.at
		if e.at.Before(at(0, 30)) {
			seqTo = seq
		}
	}
	salts := &counter{}
	prev := ""
	for _, b := range []struct {
		day      string
		from, to int64
	}{{"2026-11-19", 0, seqTo}, {"2026-11-20", seqTo, int64(len(f.rows))}} {
		rows := f.rows[b.from:b.to]
		blk, list, err := evidence.HashBlock(rows, salts)
		if err != nil {
			t.Fatal(err)
		}
		d := evidence.Day{DeviceID: devA, Day: b.day, PrevChainHash: prev, ContentHash: blk.ContentHash, RulesVersion: struggle.RulesVersion,
			SeqFrom: b.from, SeqTo: b.to, Buckets: blk.Buckets, TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo}
		chain, sig, err := evidence.Sign(f.priv, d)
		if err != nil {
			t.Fatal(err)
		}
		fb := fxBlock{list: list, byKey: map[uint64][]evidence.Row{}, rec: EvidenceRecord{
			Day: b.day, PrevChainHash: prev, ContentHash: blk.ContentHash, ChainHash: chain, Signature: sig,
			RulesVersion: struggle.RulesVersion, EventCounts: blk.Counts, SeqFrom: b.from, SeqTo: b.to, Buckets: blk.Buckets,
			TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo,
		}}
		for _, r := range rows {
			k, err := evidence.BucketKey(r)
			if err != nil {
				t.Fatal(err)
			}
			fb.byKey[k] = append(fb.byKey[k], r)
		}
		f.blocks = append(f.blocks, fb)
		f.dev.Records = append(f.dev.Records, fb.rec)
		prev = chain
	}

	// The agent's runs: minute m at T = m rounded to 10 minutes + 40.
	for m := at(0, 0); m.Before(at(1, 0)); m = m.Add(time.Minute) {
		T := m.Truncate(10 * time.Minute).Add(40 * time.Minute)
		var H int64
		for seq, lt := range logTime {
			if lt.Before(T) && seq > H {
				H = seq
			}
		}
		res, err := replay.Run(f.rows, replay.Key{T: T, H: H}, struggle.RulesVersion, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Minutes {
			if !r.Minute.Equal(m) || r.Project == "" {
				continue
			}
			row := Minute{DeviceID: devA, Minute: m, Project: r.Project, Label: string(r.Label), Credited: r.Credited,
				Cuts: sortedCopy(r.Cuts), Flags: sortedCopy(r.Flags), RulesVersion: struggle.RulesVersion,
				ComputedAt: ptr(T), Snapshot: ptr(H)}
			if r.Project == "app" {
				f.frozen = append(f.frozen, row)
			} else {
				f.lib = append(f.lib, row)
			}
		}
	}
	return f
}

func ptr[T any](v T) *T { return &v }

// labels counts the frozen rows by label.
func (f *fixture) labels() map[string]int {
	out := map[string]int{}
	for _, r := range f.frozen {
		out[r.Label]++
	}
	return out
}

// partOpts changes how part builds a proof, to tamper with it.
type partOpts struct {
	// rows are the frozen rows the part backs (all when nil).
	rows []Minute
	// hide are minute bucket keys left unopened in every block.
	hide []uint64
	// dropEdges are leaf keys never revealed as edges.
	dropEdges []uint64
	// noRepo leaves every repo bucket unopened.
	noRepo bool
	// skipDays leaves those blocks out.
	skipDays []string
}

// part builds device A's part for rows the way the agent does
// (crediting.BuildProofs): for each block, every minute bucket of each
// key's window when the block needs completeness, with the edges, and
// the repo bucket of the block holding H.
func (f *fixture) part(t testing.TB, o partOpts) Part {
	t.Helper()
	rows := o.rows
	if rows == nil {
		rows = f.frozen
	}
	p := Part{DeviceID: devA}
	var keys []replay.Key
	for _, r := range rows {
		k, ok := r.Key()
		if !ok {
			continue
		}
		keys = append(keys, k)
		p.Minutes = append(p.Minutes, PartMinute{Minute: r.Minute, ComputedAt: k.T, Snapshot: k.H})
	}
	for _, b := range f.blocks {
		if slices.Contains(o.skipDays, b.rec.Day) {
			continue
		}
		d := b.rec.Signed(devA)
		var rs []evidence.MinuteRange
		repo := false
		for _, k := range keys {
			if w := window(k); evidence.NeedsCompleteness(d, w, k.H) {
				rs = append(rs, w)
			}
			repo = repo || (d.SeqFrom < k.H && k.H <= d.SeqTo)
		}
		if len(rs) == 0 && !repo {
			continue
		}
		open, edges := evidence.PlanProof(b.list, rs)
		if repo && !o.noRepo {
			open = append(open, 0)
			edges = slices.DeleteFunc(edges, func(i int) bool { return i == 0 })
		}
		opened := map[int][]evidence.Row{}
		for _, i := range open {
			if !slices.Contains(o.hide, b.list[i].Key) {
				opened[i] = b.byKey[b.list[i].Key]
			}
		}
		edges = slices.DeleteFunc(edges, func(i int) bool { return slices.Contains(o.dropEdges, b.list[i].Key) })
		proof, err := evidence.BuildProof(b.list, opened, edges)
		if err != nil {
			t.Fatal(err)
		}
		p.Blocks = append(p.Blocks, Block{Day: b.rec.Day, Proof: proof})
	}
	return p
}
