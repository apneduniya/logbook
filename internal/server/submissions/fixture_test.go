package submissions_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/devices"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/submission"
	"github.com/apneduniya/logbook/pkg/wire"
)

// The submitted day and the clock: the day is sealed two days before now.
var (
	submittedDay = "2026-11-20"
	midnight     = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	registered   = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	now          = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
)

// at is a time on the submitted day.
func at(h, m int) time.Time {
	return midnight.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
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

// evDevice is a device whose log and sealed block the test controls: an
// honest agent's evidence for the submitted day.
type evDevice struct {
	id    uuid.UUID
	priv  ed25519.PrivateKey
	rows  []evidence.Row
	list  []evidence.Bucket
	byKey map[uint64][]evidence.Row
	rec   evidence.Day
	// minutes are the rows the agent computed, every project.
	minutes []submission.Minute
}

// register registers a device for user with a key from seed.
func (e *env) register(user uuid.UUID, name string, seed byte) *evDevice {
	e.t.Helper()
	priv := ed25519.NewKeyFromSeed(slices.Repeat([]byte{seed}, ed25519.SeedSize))
	out, err := devices.NewService(e.st, devices.WithClock(func() time.Time { return registered })).Register(
		context.Background(), user, devices.Registration{Name: name, OS: "linux", PublicKey: priv.Public().(ed25519.PublicKey)})
	if err != nil {
		e.t.Fatal(err)
	}
	return &evDevice{id: out.ID, priv: priv}
}

// work logs the person's work on the submitted day: repos at 08:00, then
// from 09:00 to 10:00 activity every 15 s and an app heartbeat each minute,
// a lib heartbeat in 09:05 to 09:07 (another project), a Claude Code turn
// from 09:24 to 09:26, and a commit at 09:20. It seals it all in one block
// (seq 0 to N), uploads the record through the ledger, and computes every
// minute of 09:00 to 10:00 the way the agent does: at T = the minute rounded
// down to 10 minutes plus 40, with H the last row logged before T.
func (e *env) work(user uuid.UUID, d *evDevice) {
	e.t.Helper()
	type logged struct {
		at  time.Time
		row func(seq int64) evidence.Row
	}
	var evs []logged
	add := func(when time.Time, row func(int64) evidence.Row) { evs = append(evs, logged{when, row}) }
	add(at(8, 0), func(s int64) evidence.Row {
		return evidence.RepoRow(s, at(8, 0), evidence.Repo{Path: "/src/app", Project: "app"})
	})
	add(at(8, 0), func(s int64) evidence.Row {
		return evidence.RepoRow(s, at(8, 0), evidence.Repo{Path: "/src/lib", Project: "lib"})
	})
	for s := at(9, 0); s.Before(at(10, 0)); s = s.Add(15 * time.Second) {
		add(s, func(seq int64) evidence.Row {
			return evidence.ActivityRow(seq, wire.ActivitySample{Time: s, IdleSeconds: 1, FocusApp: "Code", FocusKind: "editor"})
		})
		if s.Second() == 0 {
			add(s, func(seq int64) evidence.Row {
				return evidence.HeartbeatRow(seq, wire.Heartbeat{Entity: "/src/app/main.go", Time: float64(s.Unix()) + 0.5,
					Project: "app", IsWrite: s.Minute()%3 == 0, Category: "coding"})
			})
		}
		if s.Second() == 30 && !s.Before(at(9, 5)) && s.Before(at(9, 8)) {
			add(s, func(seq int64) evidence.Row {
				return evidence.HeartbeatRow(seq, wire.Heartbeat{Entity: "/src/lib/x.go", Time: float64(s.Unix()), Project: "lib", IsWrite: true})
			})
		}
	}
	ai := func(when time.Time, kind wire.AIEventKind) {
		add(when, func(seq int64) evidence.Row {
			ev := wire.AIEvent{Time: when, Harness: wire.HarnessClaudeCode, Kind: kind, SessionID: "secret-session", Cwd: "/src/app", Model: "claude-x"}
			if kind == wire.AIPromptSubmit {
				ev.PromptChars, ev.PromptHash = 80, "deadbeefcafe"
			}
			return evidence.AIRow(seq, ev)
		})
	}
	ai(at(9, 24), wire.AIPromptSubmit)
	ai(at(9, 26), wire.AITurnEnd)
	add(at(9, 20), func(seq int64) evidence.Row {
		return evidence.GitRow(seq, wire.GitEvent{Time: at(9, 20), Project: "app", Kind: "commit",
			CommitHash: "0123456789abcdef0123456789abcdef01234567", FilesChanged: 2, Insertions: 30, Deletions: 4})
	})
	slices.SortStableFunc(evs, func(a, b logged) int { return a.at.Compare(b.at) })
	logTime := map[int64]time.Time{}
	for i, ev := range evs {
		seq := int64(i + 1)
		d.rows = append(d.rows, ev.row(seq))
		logTime[seq] = ev.at
	}

	blk, list, err := evidence.HashBlock(d.rows, &counter{})
	if err != nil {
		e.t.Fatal(err)
	}
	d.list = list
	d.byKey = map[uint64][]evidence.Row{}
	for _, r := range d.rows {
		k, err := evidence.BucketKey(r)
		if err != nil {
			e.t.Fatal(err)
		}
		d.byKey[k] = append(d.byKey[k], r)
	}
	d.rec = evidence.Day{DeviceID: d.id.String(), Day: submittedDay, ContentHash: blk.ContentHash, RulesVersion: struggle.RulesVersion,
		SeqFrom: 0, SeqTo: int64(len(d.rows)), Buckets: blk.Buckets, TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo}
	e.seal(user, d.id, d.priv, d.rec, blk.Counts)

	for m := at(9, 0); m.Before(at(10, 0)); m = m.Add(time.Minute) {
		T := m.Truncate(10 * time.Minute).Add(40 * time.Minute)
		var H int64
		for seq, lt := range logTime {
			if lt.Before(T) && seq > H {
				H = seq
			}
		}
		res, err := replay.Run(d.rows, replay.Key{T: T, H: H}, struggle.RulesVersion, nil)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, r := range res.Minutes {
			if !r.Minute.Equal(m) || r.Project == "" {
				continue
			}
			tt, hh := T, H
			row := submission.Minute{DeviceID: d.id.String(), Minute: m, Project: r.Project, Label: string(r.Label),
				Credited: r.Credited, Cuts: nonNil(r.Cuts), Flags: nonNil(r.Flags), RulesVersion: struggle.RulesVersion,
				ComputedAt: &tt, Snapshot: &hh}
			d.minutes = append(d.minutes, row)
			e.credit(user, row)
		}
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// seal stores a signed evidence record through the ledger.
func (e *env) seal(user, device uuid.UUID, priv ed25519.PrivateKey, d evidence.Day, counts map[string]int) {
	e.t.Helper()
	chain, sig, err := evidence.Sign(priv, d)
	if err != nil {
		e.t.Fatal(err)
	}
	seqFrom, seqTo, buckets := d.SeqFrom, d.SeqTo, int64(d.Buckets)
	_, err = ledger.NewService(e.st, ledger.WithClock(func() time.Time { return now })).Put(context.Background(), user, device, ledger.Upload{
		Day: d.Day, PrevChainHash: d.PrevChainHash, ContentHash: d.ContentHash, ChainHash: chain,
		Signature: base64.StdEncoding.EncodeToString(sig), RulesVersion: d.RulesVersion, EventCounts: counts,
		SeqFrom: &seqFrom, SeqTo: &seqTo, Buckets: &buckets, TimeFrom: d.TimeFrom, TimeTo: d.TimeTo,
	})
	if err != nil {
		e.t.Fatalf("sealing %s: %v", d.Day, err)
	}
}

// sealOther stores a record for a device whose evidence the test doesn't
// back: a block of 10 rows with an arbitrary content hash.
func (e *env) sealOther(user uuid.UUID, d *evDevice) {
	e.t.Helper()
	from, to := at(9, 0), at(10, 0)
	e.seal(user, d.id, d.priv, evidence.Day{DeviceID: d.id.String(), Day: submittedDay,
		ContentHash: "aa00000000000000000000000000000000000000000000000000000000000000", RulesVersion: struggle.RulesVersion,
		SeqFrom: 0, SeqTo: 10, Buckets: 3, TimeFrom: &from, TimeTo: &to}, map[string]int{"activity": 10})
}

// credit stores a credited row as the minutes endpoint would.
func (e *env) credit(user uuid.UUID, m submission.Minute) {
	e.t.Helper()
	device := uuid.MustParse(m.DeviceID)
	if _, err := e.st.Pool.Exec(context.Background(), `
		INSERT INTO credited_minutes (device_id, user_id, minute, project, label, credited, cuts, flags, rules_version,
		                              computed_at, snapshot)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		device, user, m.Minute, m.Project, m.Label, m.Credited, m.Cuts, m.Flags, m.RulesVersion, m.ComputedAt, m.Snapshot); err != nil {
		e.t.Fatalf("crediting %v: %v", m.Minute, err)
	}
}

// window is the minutes a run at k read.
func window(k replay.Key) evidence.MinuteRange {
	s := k.Span()
	return evidence.MinuteRange{From: s.History, To: s.To}
}

// partOpts changes how part builds a proof.
type partOpts struct {
	// project limits the backed rows to one project ("" for every row).
	project string
	// tamper edits the first opened line of a minute bucket.
	tamper bool
}

// part builds the device's part as the agent does: the minutes of
// project it backs, and one proof opening every minute bucket of each
// key's window, with the edges, and the repo bucket.
func (d *evDevice) part(t *testing.T, o partOpts) submission.Part {
	t.Helper()
	p := submission.Part{DeviceID: d.id.String(), Blocks: []submission.Block{}, Minutes: []submission.PartMinute{}}
	var ranges []evidence.MinuteRange
	repo := false
	for _, m := range d.minutes {
		if o.project != "" && m.Project != o.project {
			continue
		}
		k, _ := m.Key()
		p.Minutes = append(p.Minutes, submission.PartMinute{Minute: m.Minute, ComputedAt: k.T, Snapshot: k.H})
		if w := window(k); evidence.NeedsCompleteness(d.rec, w, k.H) {
			ranges = append(ranges, w)
		}
		repo = repo || (d.rec.SeqFrom < k.H && k.H <= d.rec.SeqTo)
	}
	open, edges := evidence.PlanProof(d.list, ranges)
	if repo {
		open = append(open, 0)
		edges = slices.DeleteFunc(edges, func(i int) bool { return i == 0 })
	}
	opened := map[int][]evidence.Row{}
	for _, i := range open {
		opened[i] = d.byKey[d.list[i].Key]
	}
	proof, err := evidence.BuildProof(d.list, opened, edges)
	if err != nil {
		t.Fatal(err)
	}
	if o.tamper {
		for i := range proof.Opened {
			if proof.Opened[i].Key != evidence.RepoKey && len(proof.Opened[i].Lines) > 0 {
				proof.Opened[i].Lines = proof.Opened[i].Lines[1:]
				break
			}
		}
	}
	p.Blocks = append(p.Blocks, submission.Block{Day: submittedDay, Proof: proof})
	return p
}
