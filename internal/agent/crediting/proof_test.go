package crediting

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// recheck does what the server's re-check does with proofs for keys (ADR
// 0020): it verifies each proof against the record the server stored for
// its day (signed and chained on upload), checks every window that needs
// it is proven complete, and returns the rows the proofs reveal.
func recheck(t *testing.T, e *env, p Proofs, keys []replay.Key) []evidence.Row {
	t.Helper()
	held := map[string]storedDay{}
	for _, d := range e.srv.storedDays(e.deviceID()) {
		held[d.Day] = d
	}
	var rows []evidence.Row
	for _, bp := range p.Blocks {
		s, ok := held[bp.Record.Day]
		if !ok || s.ChainHash != bp.ChainHash {
			t.Fatalf("proof for %s, which the server doesn't hold as %s", bp.Record.Day, bp.ChainHash)
		}
		rec := evidence.Day{DeviceID: e.deviceID(), Day: s.Day, PrevChainHash: s.PrevChainHash, ContentHash: s.ContentHash,
			RulesVersion: s.RulesVersion, SeqFrom: s.SeqFrom, SeqTo: s.SeqTo, Buckets: s.Buckets, TimeFrom: s.TimeFrom, TimeTo: s.TimeTo}
		v, err := evidence.VerifyProof(bp.Proof, rec)
		if err != nil {
			t.Fatalf("proof for %s: %v", rec.Day, err)
		}
		for _, k := range keys {
			if w := window(k); evidence.NeedsCompleteness(rec, w, k.H) {
				if err := v.CheckComplete(w); err != nil {
					t.Fatalf("%s for the key %s/%d: %v", rec.Day, k.T.Format("15:04"), k.H, err)
				}
			}
		}
		rows = append(rows, v.Rows()...)
	}
	return rows
}

// keyOf returns the key of the stored rows of minute m.
func keyOf(t *testing.T, e *env, m time.Time) (replay.Key, []localdb.CreditedMinute) {
	t.Helper()
	rows, err := e.db.StoredMinutes(context.Background(), m, m.Add(time.Minute))
	if err != nil || len(rows) == 0 {
		t.Fatalf("minute %s: %+v, %v", m.Format("15:04"), rows, err)
	}
	return replay.Key{T: rows[0].Key.T, H: rows[0].Key.H}, rows
}

// Hidden buckets are proven from the bucket list, so pruning their rows
// doesn't matter; a minute whose window needs a pruned bucket opened goes
// without evidence.
func TestProofsSurvivePruning(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	// Retention ends at 12:00 on the 19th when pruning runs below.
	e.dbOptions = []localdb.Option{localdb.WithRetention(14 * time.Hour)}
	e.open()
	e.run()
	addDay(t, e.db, "2026-11-19") // 09:00, 09:01 and 10:00
	evening := time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC)
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{
		{Type: "activity", Activity: &wire.ActivitySample{Time: evening, FocusKind: "editor"}},
		{Type: "activity", Activity: &wire.ActivitySample{Time: evening.Add(time.Minute), FocusKind: "editor"}},
	}); err != nil {
		t.Fatal(err)
	}
	e.set(time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC))
	day := untilSealed(t, e, "2026-11-19")
	if res, err := e.db.Prune(ctx); err != nil || res.ActivitySamples != 1 || res.AIEvents != 1 {
		t.Fatalf("Prune = %+v, %v", res, err)
	}
	if res, err := e.db.PruneCrediting(ctx); err != nil || res.Heartbeats != 1 {
		t.Fatalf("PruneCrediting = %+v, %v", res, err)
	}

	late := replay.Key{T: evening.Add(30 * time.Minute), H: day.SeqTo}
	early := replay.Key{T: time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC), H: day.SeqTo}
	p, err := BuildProofs(ctx, e.db, e.deviceID(), []replay.Key{early, late})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Unbacked) != 1 || p.Unbacked[0] != early || len(p.Blocks) != 1 {
		t.Fatalf("proofs = %+v; want only the morning's key unbacked", p)
	}
	rows := recheck(t, e, p, []replay.Key{late})
	if len(rows) != 2 || !rows[0].Time.Equal(evening) {
		t.Fatalf("revealed rows = %+v; want the evening's two samples", rows)
	}
	if got := p.Blocks[0].Proof; len(got.Opened) != 3 || len(got.Edges) != 1 || len(got.Hashes) == 0 {
		t.Fatalf("proof = %+v; want the repo bucket and the evening's two minutes opened, the 10:00 bucket as an edge", got)
	}
}

// A key nothing sealed can back is unbacked: one whose H no accepted block
// holds, and one whose window read rows logged before the device's chain
// started (the previous device sealed them).
func TestProofsUnbackedOutsideTheChain(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	e.run()
	addDay(t, e.db, "2026-11-19")
	before := time.Date(2026, 11, 20, 0, 10, 0, 0, time.UTC)
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity", Activity: &wire.ActivitySample{Time: before, FocusKind: "editor"}}}); err != nil {
		t.Fatal(err)
	}
	e.set(time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC))
	day := untilSealed(t, e, "2026-11-19")
	k := replay.Key{T: time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC), H: day.SeqTo}
	unsealed := replay.Key{T: k.T, H: day.SeqTo + 1}
	p, err := BuildProofs(ctx, e.db, e.deviceID(), []replay.Key{k, unsealed})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Unbacked, []replay.Key{unsealed}) {
		t.Fatalf("unbacked = %+v", p.Unbacked)
	}
	if rows := recheck(t, e, p, []replay.Key{k}); len(rows) != 2 {
		t.Fatalf("rows = %+v; want 09:00 and 09:01", rows)
	}

	// Another account takes over; its chain starts after the 00:10 sample.
	e.credit(time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC), "app", 60)
	e.setKey("key-2")
	for range 2 {
		e.advance(2 * time.Second)
		e.run()
	}
	after := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity", Activity: &wire.ActivitySample{Time: after, FocusKind: "editor"}}}); err != nil {
		t.Fatal(err)
	}
	e.set(time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC))
	first := untilSealed(t, e, "2026-11-20")
	reads := replay.Key{T: time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC), H: first.SeqTo} // reads 00:10
	later := replay.Key{T: time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC), H: first.SeqTo}
	p, err = BuildProofs(ctx, e.db, e.deviceID(), []replay.Key{reads, later})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Unbacked, []replay.Key{reads}) {
		t.Fatalf("unbacked = %+v; want the key that read the previous device's row", p.Unbacked)
	}
	if rows := recheck(t, e, p, []replay.Key{later}); len(rows) != 1 || !rows[0].Time.Equal(after) {
		t.Fatalf("rows = %+v", rows)
	}
}

// A block that follows one sealed before the agent kept bucket lists (an
// accepted pre-0019 day) doesn't open with a repo snapshot, so its repo
// bucket lacks paths mapped before it started: a key whose H it holds is
// unbacked, even though every row its window read is in the block. The
// next block opens with a snapshot and backs its keys again.
func TestProofsUnbackedAfterPre0019Block(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	e.run()
	if err := e.db.RecordRepos(ctx, e.clock(), []struggle.Repo{{Path: "/src/app", Project: "app"}}); err != nil {
		t.Fatal(err)
	}
	keys := map[string]replay.Key{}
	for _, day := range []string{"2026-11-19", "2026-11-20", "2026-11-21"} {
		addDay(t, e.db, day)
		start, end, err := evidence.DayBounds(day)
		if err != nil {
			t.Fatal(err)
		}
		e.set(end.Add(time.Hour + 5*time.Second))
		d := untilSealed(t, e, day)
		keys[day] = replay.Key{T: start.Add(9*time.Hour + 30*time.Minute), H: d.SeqTo}
	}
	all := []replay.Key{keys["2026-11-20"], keys["2026-11-21"]}
	if p, err := BuildProofs(ctx, e.db, e.deviceID(), all); err != nil || len(p.Unbacked) != 0 {
		t.Fatalf("before = %+v, %v; want both keys backed", p, err)
	}

	// Make the 19th what local migration 0008 leaves of a day sealed by the
	// previous agent: accepted, with no bucket list.
	raw, err := sql.Open("sqlite", config.DBPath(e.dir)+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, q := range []string{
		"UPDATE evidence_days SET buckets = 0, time_from = NULL, time_to = NULL WHERE day = '2026-11-19'",
		"DELETE FROM evidence_buckets WHERE day = '2026-11-19'",
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	p, err := BuildProofs(ctx, e.db, e.deviceID(), all)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Unbacked, []replay.Key{keys["2026-11-20"]}) {
		t.Fatalf("unbacked = %+v; want only the key in the block after the pre-0019 day", p.Unbacked)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Record.Day != "2026-11-21" {
		t.Fatalf("proofs = %+v; want only the 21st's", p.Blocks)
	}
	if rows := recheck(t, e, p, []replay.Key{keys["2026-11-21"]}); !slices.ContainsFunc(rows, func(r evidence.Row) bool {
		repo, ok := r.Value.(evidence.Repo)
		return ok && repo.Path == "/src/app"
	}) {
		t.Fatalf("rows = %+v; want the 21st's repo snapshot of /src/app", rows)
	}
}

// The property test of ADR 0019 and ADR 0020: for random logs computed and
// sealed by the real engine (rows hashed over several runs, repo mappings
// changing, late events, and the 19th sealed while minutes of the 20th
// are still computed, so windows around midnight span two blocks), proofs
// built for random final minutes' keys verify against the signed records,
// prove every window complete, and pkg/replay at each key over the
// revealed rows reproduces the agent's minute.
func TestProofsReplayFinalMinutes(t *testing.T) {
	for seed := range uint64(propertySeeds) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rng := rand.New(rand.NewPCG(seed, 19))
			start := time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC)
			end := start.Add(4 * time.Hour)
			var stream []delivery
			for from := start; from.Before(end); from = from.Add(2 * time.Hour) {
				part, _, _ := randomStream(rng, from, from.Add(2*time.Hour))
				stream = append(stream, part...)
			}
			slices.SortStableFunc(stream, func(a, b delivery) int { return a.at.Compare(b.at) })

			e := newEnv(t, start.Add(5*time.Second))
			sealRows := 50 + rng.IntN(500)
			e.options = func(o *Options) { o.Compute, o.SealRows = struggle.Compute, sealRows }
			e.open()
			next := 0
			for now := start.Add(5 * time.Second); now.Before(end.Add(time.Hour)); now = now.Add(time.Minute) {
				e.set(now)
				for ; next < len(stream) && !stream[next].at.After(now); next++ {
					d := stream[next]
					var err error
					if d.hbs != nil {
						err = e.db.Enqueue(ctx, d.hbs, false)
					} else {
						err = e.db.InsertEvents(ctx, []wire.LocalEvent{d.ev})
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if rng.IntN(20) == 0 {
					project := []string{"app", "renamed", "lib"}[rng.IntN(3)]
					if err := e.db.RecordRepos(ctx, now, []struggle.Repo{{Path: "/src/app", Project: project}}); err != nil {
						t.Fatal(err)
					}
				}
				e.run()
			}
			if days := e.srv.storedDays(e.deviceID()); len(days) != 1 || days[0].Day != "2026-11-19" {
				t.Fatalf("server days = %+v, want the 19th sealed in the loop", days)
			}
			e.set(time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC))
			untilSealed(t, e, "2026-11-20")

			stored, err := e.db.StoredMinutes(ctx, start, end)
			if err != nil {
				t.Fatal(err)
			}
			var minutes []time.Time
			for _, r := range stored {
				if !slices.ContainsFunc(minutes, r.Minute.Equal) {
					minutes = append(minutes, r.Minute)
				}
			}
			if len(minutes) < 10 {
				t.Fatalf("only %d minutes credited", len(minutes))
			}
			rng.Shuffle(len(minutes), func(i, j int) { minutes[i], minutes[j] = minutes[j], minutes[i] })
			minutes = minutes[:min(len(minutes), 40)]
			var keys []replay.Key
			for _, m := range minutes {
				k, _ := keyOf(t, e, m)
				keys = append(keys, k)
			}
			// One bundle for all the keys, as a submission builds it.
			all, err := BuildProofs(ctx, e.db, e.deviceID(), keys)
			if err != nil {
				t.Fatal(err)
			}
			if len(all.Unbacked) != 0 {
				t.Fatalf("unbacked keys %+v", all.Unbacked)
			}
			revealed := recheck(t, e, all, keys)
			if len(all.Blocks) != 2 {
				t.Fatalf("%d blocks proven, want both days'", len(all.Blocks))
			}
			for _, m := range minutes {
				k, rows := keyOf(t, e, m)
				res, err := replay.Run(revealed, k, struggle.RulesVersion, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				// And from a proof of this key alone, revealing less.
				own, err := BuildProofs(ctx, e.db, e.deviceID(), []replay.Key{k})
				if err != nil || len(own.Unbacked) != 0 {
					t.Fatalf("proof for %s: %+v, %v", m.Format("15:04"), own.Unbacked, err)
				}
				alone, err := replay.Run(recheck(t, e, own, []replay.Key{k}), k, struggle.RulesVersion, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				want := credits(rows)
				if got := replay.Credits(res, m); !replay.Same(got, want) {
					t.Fatalf("minute %s at %s/%d: agent %+v, replay of the bundle %+v", m.Format("15:04"), k.T.Format("15:04"), k.H, want, got)
				}
				if got := replay.Credits(alone, m); !replay.Same(got, want) {
					t.Fatalf("minute %s at %s/%d: agent %+v, replay of its own proof %+v", m.Format("15:04"), k.T.Format("15:04"), k.H, want, got)
				}
			}
		})
	}
}

// credits is stored rows as a replay compares them.
func credits(rows []localdb.CreditedMinute) []replay.Credit {
	out := make([]replay.Credit, len(rows))
	for i, r := range rows {
		out[i] = replay.Credit{Project: r.Project, Label: r.Label, Credited: r.Credited, Cuts: r.Cuts}
	}
	return out
}

// A key that read a row below a takeover baseline stays unbacked after
// retention deletes that row: the gap can't be shown to be clear once rows
// of the window may be gone.
func TestProofsUnbackedAfterGapPruned(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 20, 0, 0, 5, 0, time.UTC))
	// Retention ends at 01:00 on the 20th when pruning runs below.
	e.dbOptions = []localdb.Option{localdb.WithRetention(24 * time.Hour)}
	e.open()
	e.run()
	before := time.Date(2026, 11, 20, 0, 10, 0, 0, time.UTC)
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity", Activity: &wire.ActivitySample{Time: before, FocusKind: "editor"}}}); err != nil {
		t.Fatal(err)
	}
	// Another account takes over; its chain starts after the 00:10 sample.
	e.set(time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC))
	e.credit(time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC), "app", 60)
	e.setKey("key-2")
	for range 2 {
		e.advance(2 * time.Second)
		e.run()
	}
	after := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity", Activity: &wire.ActivitySample{Time: after, FocusKind: "editor"}}}); err != nil {
		t.Fatal(err)
	}
	e.set(time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC))
	first := untilSealed(t, e, "2026-11-20")
	reads := replay.Key{T: time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC), H: first.SeqTo} // reads 00:10
	later := replay.Key{T: time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC), H: first.SeqTo}

	if res, err := e.db.Prune(ctx); err != nil || res.ActivitySamples != 1 {
		t.Fatalf("Prune = %+v, %v; want the 00:10 sample deleted", res, err)
	}
	p, err := BuildProofs(ctx, e.db, e.deviceID(), []replay.Key{reads, later})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Unbacked, []replay.Key{reads}) {
		t.Fatalf("unbacked = %+v; want the key whose window may have lost rows", p.Unbacked)
	}
}
