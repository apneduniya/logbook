package localdb

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

const sealDev = "0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b"

// view runs fn on a snapshot and fails the test on error.
func view(t *testing.T, d *DB, fn func(s *Snapshot) error) {
	t.Helper()
	if err := d.View(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

// A block is sealed in three transactions: the reservation freezes its
// bound and logs the repo snapshot, steps store buckets as they close, and
// completion stores the record and only then lets retention delete its
// rows.
func TestSealingLifecycle(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t, WithRetention(time.Minute))
	if err := d.RecordRepos(ctx, minute(-90), []struggle.Repo{{Path: "/src/lib", Project: "lib"}, {Path: "/src/app", Project: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertEvents(ctx, []wire.LocalEvent{activity(minute(-30)), activity(minute(-30).Add(5 * time.Second)), activity(minute(-10))}); err != nil {
		t.Fatal(err)
	}
	h := high(ctx, t, d) // 2 repo rows, 3 samples

	s, ok, err := d.ReserveSeal(ctx, sealDev, "2026-11-20", "", 0, KV{"crediting.sealed_through", "2026-11-20"})
	if err != nil || !ok || s.SeqFrom != 0 || s.SeqTo != h || s.Cursor != 0 || s.Buckets != 0 || s.Hasher != nil {
		t.Fatalf("ReserveSeal = %+v, %v, %v", s, ok, err)
	}
	if v, _ := d.Get(ctx, "crediting.sealed_through"); v != "2026-11-20" {
		t.Fatalf("mark = %q", v)
	}
	// The snapshot: every path's mapping again, after the bound, in path order.
	var snap []evidence.Row
	view(t, d, func(sn *Snapshot) error {
		var err error
		snap, err = sn.RepoRows(ctx, h, sn.H)
		return err
	})
	if len(snap) != 2 || snap[0].Seq != h+1 || snap[0].Value != (evidence.Repo{Path: "/src/app", Project: "app"}) ||
		snap[1].Value != (evidence.Repo{Path: "/src/lib", Project: "lib"}) || high(ctx, t, d) != h+2 {
		t.Fatalf("snapshot rows = %+v", snap)
	}
	if _, _, err := d.ReserveSeal(ctx, sealDev, "2026-11-21", "", h); !errors.Is(err, ErrSealing) {
		t.Fatal("a second block was reserved while one is being sealed")
	}
	if got, ok, err := d.Sealing(ctx); err != nil || !ok || got.SeqTo != s.SeqTo || got.Day != s.Day {
		t.Fatalf("Sealing = %+v, %v, %v", got, ok, err)
	}
	// Nothing of the range may be pruned before the block is stored.
	if res, err := d.Prune(ctx); err != nil || res.ActivitySamples != 0 {
		t.Fatalf("Prune while sealing = %+v, %v", res, err)
	}

	// Hash in two steps: the repo bucket, then the minutes.
	var rows []evidence.Row
	view(t, d, func(sn *Snapshot) error {
		var err error
		rows, err = sn.Rows(ctx, s.SeqFrom, s.SeqTo)
		return err
	})
	sorted, err := evidence.SortForHashing(rows)
	if err != nil {
		t.Fatal(err)
	}
	hasher := evidence.NewHasher(nil)
	closed, err := hasher.Add(sorted[:2])
	if err != nil {
		t.Fatal(err)
	}
	state, err := hasher.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveSealStep(ctx, s, 1, state, closed); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveSealStep(ctx, s, 1, state, closed); !errors.Is(err, ErrSealing) {
		t.Fatalf("a stale step was saved: %v", err)
	}
	s, _, _ = d.Sealing(ctx)
	if s.Cursor != 1 || !slices.Equal(s.Hasher, state) || s.Buckets != len(closed) {
		t.Fatalf("after a step = %+v", s)
	}
	more, err := hasher.Add(sorted[2:])
	if err != nil {
		t.Fatal(err)
	}
	blk, rest, err := hasher.Finish()
	if err != nil {
		t.Fatal(err)
	}
	last := append(more, rest...)
	e := EvidenceDay{DeviceID: sealDev, Day: s.Day, ContentHash: blk.ContentHash, ChainHash: "c", Signature: []byte{1},
		RulesVersion: "v1", EventCounts: blk.Counts, SeqFrom: s.SeqFrom + 1, SeqTo: s.SeqTo, Buckets: blk.Buckets,
		TimeFrom: blk.TimeFrom, TimeTo: blk.TimeTo, State: EvidencePending}
	wrong := e
	wrong.Buckets++
	if err := d.CompleteSeal(ctx, s, last, wrong); !errors.Is(err, ErrSealing) {
		t.Fatalf("completed with the wrong bucket count: %v", err)
	}
	if err := d.CompleteSeal(ctx, s, last, e, KV{"evidence.sealing", ""}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := d.EvidenceDayOf(ctx, sealDev, s.Day)
	if err != nil || !ok || got.Buckets != 3 || got.TimeFrom == nil || !got.TimeFrom.Equal(minute(-30)) ||
		!got.TimeTo.Equal(minute(-10)) || !got.Provable() || got.Record().SeqFrom != 0 || got.Record().SeqTo != h {
		t.Fatalf("stored day = %+v, %v, %v", got, ok, err)
	}
	var list []evidence.Bucket
	view(t, d, func(sn *Snapshot) error {
		var err error
		list, err = sn.Buckets(ctx, sealDev, s.Day)
		return err
	})
	if root, err := evidence.Root(list); err != nil || root.String() != blk.ContentHash || len(list) != blk.Buckets {
		t.Fatalf("stored bucket list = %+v: root %v, %v; want %s", list, root, err, blk.ContentHash)
	}
	if _, ok, _ := d.Sealing(ctx); ok {
		t.Fatal("the reservation outlived its block")
	}
	if n, _ := d.Counter(ctx, KeySealed); n != h {
		t.Fatalf("sealed = %d, want %d", n, h)
	}
	if res, err := d.Prune(ctx); err != nil || res.ActivitySamples != 3 {
		t.Fatalf("Prune after sealing = %+v, %v", res, err)
	}
	if err := d.CompleteSeal(ctx, s, nil, e); !errors.Is(err, ErrSealing) {
		t.Fatalf("completed twice: %v", err)
	}
}

// A range holding only repo rows (such as the last snapshot) isn't sealed:
// nothing is stored or logged, and the range goes on into the next block.
func TestReserveSealSkipsRangesOfRepoRows(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	if err := d.RecordRepos(ctx, minute(-5), []struggle.Repo{{Path: "/src/app", Project: "app"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.ReserveSeal(ctx, sealDev, "2026-11-20", "", 0, KV{"mark", "x"}); err != nil || ok {
		t.Fatalf("ReserveSeal = %v, %v; want a gap", ok, err)
	}
	if h := high(ctx, t, d); h != 1 {
		t.Fatalf("high-water mark = %d: a snapshot was logged for a gap", h)
	}
	if v, _ := d.Get(ctx, "mark"); v != "" {
		t.Fatalf("mark written for a gap: %q", v)
	}
	if _, ok, _ := d.Sealing(ctx); ok {
		t.Fatal("a gap was reserved")
	}
}

// A device taking over starts its chain at the log's high-water mark with a
// repo snapshot, and drops the block the previous device was sealing.
func TestStartChain(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	const next = "1c8d3f2a-4b5e-4f60-9bac-1d2e3f4a5b6c"
	if err := d.RecordRepos(ctx, minute(-5), []struggle.Repo{{Path: "/src/app", Project: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertEvents(ctx, []wire.LocalEvent{activity(minute(-3))}); err != nil {
		t.Fatal(err)
	}
	s, ok, err := d.ReserveSeal(ctx, sealDev, "2026-11-20", "", 0)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := d.SaveSealStep(ctx, s, 1, []byte("{}"), []evidence.Bucket{{Key: evidence.RepoKey}}); err != nil {
		t.Fatal(err)
	}
	h := high(ctx, t, d)
	base, err := d.StartChain(ctx, next, KV{"crediting.device", next})
	if err != nil || base != h {
		t.Fatalf("StartChain = %d, %v; want %d", base, err, h)
	}
	if got, _ := d.ChainBaseline(ctx, next); got != h {
		t.Fatalf("baseline = %d", got)
	}
	if got, _ := d.ChainBaseline(ctx, sealDev); got != 0 {
		t.Fatalf("baseline of a device that didn't take over = %d", got)
	}
	if v, _ := d.Get(ctx, "crediting.device"); v != next {
		t.Fatalf("mark = %q", v)
	}
	if _, ok, _ := d.Sealing(ctx); ok {
		t.Fatal("the previous device's block is still being sealed")
	}
	view(t, d, func(sn *Snapshot) error {
		snap, err := sn.RepoRows(ctx, base, sn.H)
		if err != nil {
			return err
		}
		if len(snap) != 1 || snap[0].Seq != base+1 || snap[0].Value != (evidence.Repo{Path: "/src/app", Project: "app"}) {
			t.Errorf("first rows of the new chain = %+v", snap)
		}
		list, err := sn.Buckets(ctx, sealDev, "2026-11-20")
		if len(list) != 0 {
			t.Errorf("buckets of the dropped block kept: %+v", list)
		}
		return err
	})
}

// MinuteRows and NextMinuteRow read rows by sequence range and evidence
// time: active time by its end, never repo rows.
func TestMinuteRows(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	if err := d.InsertEvents(ctx, []wire.LocalEvent{activity(minute(-30)), activity(minute(-20).Add(59 * time.Second))}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRepos(ctx, minute(-20), []struggle.Repo{{Path: "/src/app", Project: "app"}}); err != nil {
		t.Fatal(err)
	}
	point := ActiveTimePoint{From: minute(-21), To: minute(-19).Add(-time.Millisecond), SessionID: "s", Type: ActiveUser, Secs: 3}
	if err := d.InsertActiveTime(ctx, []ActiveTimePoint{point}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertEvents(ctx, []wire.LocalEvent{activity(minute(-20))}); err != nil {
		t.Fatal(err)
	}
	seqs := func(rows []evidence.Row) []int64 {
		var out []int64
		for _, r := range rows {
			out = append(out, r.Seq)
		}
		slices.Sort(out)
		return out
	}
	view(t, d, func(s *Snapshot) error {
		rows, err := s.MinuteRows(ctx, 0, s.H, minute(-20), minute(-19))
		if err != nil {
			return err
		}
		if got := seqs(rows); !slices.Equal(got, []int64{2, 4, 5}) {
			t.Errorf("rows of minute -20 = %v, want samples 2 and 5 and active time 4", got)
		}
		if rows, err = s.MinuteRows(ctx, 2, 4, minute(-20), minute(-19)); err != nil || !slices.Equal(seqs(rows), []int64{4}) {
			t.Errorf("rows of minute -20 in (2, 4] = %v, %v", seqs(rows), err)
		}
		for _, c := range []struct {
			after, upto int64
			from, want  time.Time
			ok          bool
		}{
			{0, 5, time.Unix(0, 0), minute(-30), true},
			{1, 5, time.Unix(0, 0), minute(-20), true},
			{0, 5, minute(-29), minute(-20), true},
			{0, 5, minute(-19), time.Time{}, false},
			{2, 3, time.Unix(0, 0), time.Time{}, false}, // only the repo row
		} {
			got, ok, err := s.NextMinuteRow(ctx, c.after, c.upto, c.from)
			if err != nil || ok != c.ok || !got.Equal(c.want) {
				t.Errorf("NextMinuteRow(%d, %d, %s) = %s, %v, %v", c.after, c.upto, c.from.Format("15:04"), got, ok, err)
			}
		}
		return nil
	})
}

// Sealing reads a block one time step at a time, so each step must walk a
// time index over that step, not the block's whole sequence range: a block
// can hold months of rows, and walking it in every step makes sealing
// quadratic in its size. The next-minute probe must also stay inside the
// index (seq is checked there).
func TestSealingStepsWalkTimeIndexes(t *testing.T) {
	d, _ := openTest(t)
	ctx := context.Background()
	timeIndex := map[string]string{
		"heartbeats": "heartbeats_time", "ai_events": "ai_events_time", "activity_samples": "activity_samples_time",
		"git_events": "git_events_time", "active_time": "active_time_to",
	}
	plan := func(q string, args ...any) []string {
		t.Helper()
		rows, err := d.sql.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(detail, "SEARCH ") || strings.HasPrefix(detail, "SCAN ") {
				out = append(out, detail)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for table, q := range map[string]string{
		"heartbeats": qHeartbeatRowsIn, "ai_events": qAIRowsIn, "activity_samples": qActivityRowsIn,
		"git_events": qGitRowsIn, "active_time": qActiveRowsIn,
	} {
		p := plan(q, 1, 2, 3, 4)
		if len(p) != 1 || !strings.Contains(p[0], "USING INDEX "+timeIndex[table]+" ") {
			t.Errorf("%s step read plan = %q, want the %s index", table, p, timeIndex[table])
		}
	}
	p := plan(qNextRowTime, 1, 2, 3)
	var walks []string
	for _, step := range p {
		if strings.Contains(step, "subquery") {
			continue
		}
		walks = append(walks, step)
	}
	if len(walks) != len(timeIndex) {
		t.Fatalf("next-minute plan = %q, want one walk per table", p)
	}
	for _, w := range walks {
		table := strings.Fields(w)[1]
		if !strings.Contains(w, "USING COVERING INDEX "+timeIndex[table]+" ") {
			t.Errorf("next-minute plan for %s = %q, want the covering %s index", table, w, timeIndex[table])
		}
	}
}
