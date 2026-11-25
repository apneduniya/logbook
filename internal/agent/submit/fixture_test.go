package submit

import (
	"context"
	"crypto/ed25519"
	"slices"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/submission"
	"github.com/apneduniya/logbook/pkg/wire"
)

const (
	laptop  = "a0000000-0000-4000-8000-000000000001"
	desktop = "b0000000-0000-4000-8000-000000000002"
)

// day is the submitted day; now is when the tests run.
var (
	day = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	now = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
)

func at(h, m, s int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
}

// counter is a deterministic salt source.
type counter struct{ n int }

func (c *counter) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c.n % 251)
		c.n++
	}
	return len(p), nil
}

// fixture is the laptop's log for one sealed block (day 2026-11-20) and
// the plan the server answers with for project app on that day.
//
// The person edits app from 10:00 to 10:30 (activity every 15 s, a
// heartbeat each minute). At 10:05 and 10:06 they also save a file of lib,
// and at 10:10 ask Claude Code something in /src/lib; at 10:12 they open a
// scratch file outside every repo. At 08:00 they save in lib too, but no
// window reads that. Every minute of 10:00 to 10:59 is computed at key
// (11:00, H), H being the last row. The plan also holds a row stored
// before keys (09:30), a minute whose key the device can't prove (12:30),
// and three minutes of the desktop.
type fixture struct {
	priv   ed25519.PrivateKey
	rows   []evidence.Row
	list   []evidence.Bucket
	byKey  map[uint64][]evidence.Row
	rec    evidence.Day
	stored submission.EvidenceRecord
	key    replay.Key
	// unprovable is the key of 12:30.
	unprovable replay.Key
	plan       submission.Plan
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{priv: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), byKey: map[uint64][]evidence.Row{}}
	add := func(row func(seq int64) evidence.Row) { f.rows = append(f.rows, row(int64(len(f.rows)+1))) }
	for _, r := range []evidence.Repo{{Path: "/src/app", Project: "app"}, {Path: "/src/lib", Project: "lib"}} {
		add(func(s int64) evidence.Row { return evidence.RepoRow(s, at(7, 0, 0), r) })
	}
	add(func(s int64) evidence.Row {
		return evidence.HeartbeatRow(s, wire.Heartbeat{Entity: "/src/lib/old.go", Time: float64(at(8, 0, 0).Unix()), IsWrite: true})
	})
	for ts := at(10, 0, 0); ts.Before(at(10, 30, 0)); ts = ts.Add(15 * time.Second) {
		add(func(s int64) evidence.Row {
			return evidence.ActivityRow(s, wire.ActivitySample{Time: ts, IdleSeconds: 1, FocusApp: "Code", FocusKind: "editor"})
		})
		if ts.Second() == 0 {
			add(func(s int64) evidence.Row {
				return evidence.HeartbeatRow(s, wire.Heartbeat{Entity: "/src/app/main.go", Time: float64(ts.Unix()) + 0.5, IsWrite: true})
			})
		}
	}
	for _, m := range []int{5, 6} {
		add(func(s int64) evidence.Row {
			return evidence.HeartbeatRow(s, wire.Heartbeat{Entity: "/src/lib/x.go", Time: float64(at(10, m, 30).Unix()), IsWrite: true})
		})
	}
	add(func(s int64) evidence.Row {
		return evidence.AIRow(s, wire.AIEvent{Time: at(10, 10, 20), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit,
			SessionID: "s1", Cwd: "/src/lib", PromptChars: 40, PromptHash: "abcd"})
	})
	add(func(s int64) evidence.Row {
		return evidence.HeartbeatRow(s, wire.Heartbeat{Entity: "/tmp/scratch.txt", Time: float64(at(10, 12, 10).Unix())})
	})
	add(func(s int64) evidence.Row {
		return evidence.GitRow(s, wire.GitEvent{Time: at(10, 20, 0), Project: "app", Kind: "commit",
			CommitHash: "0123456789abcdef0123456789abcdef01234567", FilesChanged: 1, Insertions: 3})
	})

	blk, list, err := evidence.HashBlock(f.rows, &counter{})
	if err != nil {
		t.Fatal(err)
	}
	f.list = list
	for _, r := range f.rows {
		k, err := evidence.BucketKey(r)
		if err != nil {
			t.Fatal(err)
		}
		f.byKey[k] = append(f.byKey[k], r)
	}
	n := int64(len(f.rows))
	f.rec = evidence.Day{DeviceID: laptop, Day: "2026-11-20", ContentHash: blk.ContentHash, RulesVersion: struggle.RulesVersion,
		SeqFrom: 0, SeqTo: n, Buckets: blk.Buckets, TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo}
	chain, sig, err := evidence.Sign(f.priv, f.rec)
	if err != nil {
		t.Fatal(err)
	}
	f.stored = submission.EvidenceRecord{Day: f.rec.Day, ContentHash: f.rec.ContentHash, ChainHash: chain, Signature: sig,
		RulesVersion: f.rec.RulesVersion, EventCounts: blk.Counts, SeqFrom: 0, SeqTo: n, Buckets: blk.Buckets,
		TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo}

	f.key = replay.Key{T: at(11, 0, 0), H: n}
	f.unprovable = replay.Key{T: at(13, 0, 0), H: n}
	res, err := replay.Run(f.rows, f.key, struggle.RulesVersion, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed := "2026-11-24"
	f.plan = submission.Plan{Project: "app", From: "2026-11-20", To: "2026-11-20", Devices: []submission.PlanDevice{
		{ID: laptop, Name: "laptop", SealedThrough: &sealed, ThisDevice: true},
		{ID: desktop, Name: "desktop", SealedThrough: &sealed},
	}}
	row := func(dev string, m time.Time, k *replay.Key) submission.Minute {
		r := submission.Minute{DeviceID: dev, Minute: m, Project: "app", Label: "you", Credited: 60, Cuts: []string{}, Flags: []string{},
			RulesVersion: struggle.RulesVersion}
		if k != nil {
			r.ComputedAt, r.Snapshot = &k.T, &k.H
		}
		return r
	}
	f.plan.Minutes = append(f.plan.Minutes, row(laptop, at(9, 30, 0), nil))
	for _, r := range res.Minutes {
		if r.Project != "app" {
			continue
		}
		m := row(laptop, r.Minute, &f.key)
		m.Label, m.Credited, m.Cuts, m.Flags = string(r.Label), r.Credited, sorted(r.Cuts), sorted(r.Flags)
		f.plan.Minutes = append(f.plan.Minutes, m)
	}
	f.plan.Minutes = append(f.plan.Minutes, row(laptop, at(12, 30, 0), &f.unprovable))
	dk := replay.Key{T: at(11, 0, 0), H: 9}
	for _, m := range []int{1, 2, 3} {
		f.plan.Minutes = append(f.plan.Minutes, row(desktop, at(10, m, 0), &dk))
	}
	return f
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	slices.Sort(out)
	return out
}

// own returns the laptop's plan rows.
func (f *fixture) own() []submission.Minute {
	var out []submission.Minute
	for _, m := range f.plan.Minutes {
		if m.DeviceID == laptop {
			out = append(out, m)
		}
	}
	return out
}

// prove stands in for crediting.BuildProofs over the fixture's one block:
// the unprovable key is unbacked; for the others it opens every minute
// bucket of their windows with the edges, and the repo bucket.
func (f *fixture) prove(_ context.Context, keys []replay.Key) (crediting.Proofs, error) {
	var out crediting.Proofs
	var rs []evidence.MinuteRange
	for _, k := range keys {
		if k == f.unprovable {
			out.Unbacked = append(out.Unbacked, k)
			continue
		}
		sp := k.Span()
		rs = append(rs, evidence.MinuteRange{From: sp.History, To: sp.To})
	}
	if len(rs) == 0 {
		return out, nil
	}
	open, edges := evidence.PlanProof(f.list, rs)
	open = append(open, 0)
	edges = slices.DeleteFunc(edges, func(i int) bool { return i == 0 })
	opened := map[int][]evidence.Row{}
	for _, i := range open {
		opened[i] = f.byKey[f.list[i].Key]
	}
	p, err := evidence.BuildProof(f.list, opened, edges)
	if err != nil {
		return crediting.Proofs{}, err
	}
	out.Blocks = []crediting.BlockProof{{Record: f.rec, ChainHash: f.stored.ChainHash, Proof: p}}
	return out, nil
}
