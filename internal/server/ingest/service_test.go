package ingest_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/ingest"
	"github.com/apneduniya/logbook/internal/server/ingest/rollup"
	"github.com/apneduniya/logbook/pkg/wire"
)

// base is a day before now, so random timelines stay in the past.
var base = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

func TestIngestDeduplicatesResentBatches(t *testing.T) {
	svc, st, pub := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	batch := randomTimeline(rand.New(rand.NewPCG(1, 1)), base, 50)

	n, err := svc.Ingest(ctx, user, batch, ingest.SourceEditor)
	if err != nil || n != 50 {
		t.Fatalf("first Ingest() = %d, %v; want 50, nil", n, err)
	}
	before := minutesOf(t, st, user)

	// Resend the whole batch, plus an in-batch repeat.
	n, err = svc.Ingest(ctx, user, append(slices.Clone(batch), batch[0]), ingest.SourceEditor)
	if err != nil || n != 0 {
		t.Fatalf("resent Ingest() = %d, %v; want 0, nil", n, err)
	}
	if got := countRows(t, st, "SELECT count(*) FROM heartbeats WHERE user_id = $1", user); got != 50 {
		t.Fatalf("heartbeats stored = %d, want 50", got)
	}
	if after := minutesOf(t, st, user); !reflect.DeepEqual(before, after) {
		t.Fatal("resending a batch changed the rollups")
	}
	if calls := pub.Calls(); len(calls) != 1 {
		t.Fatalf("publisher called %d times, want 1 (duplicates change nothing)", len(calls))
	}
}

func TestIngestOutOfOrderMatchesInOrder(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()

	for seed := range uint64(5) {
		rng := rand.New(rand.NewPCG(seed, 99))
		timeline := randomTimeline(rng, base, 300)

		inOrder := newUser(t, st, "UTC")
		for chunk := range slices.Chunk(timeline, wire.MaxBulkHeartbeats) {
			if _, err := svc.Ingest(ctx, inOrder, chunk, ingest.SourceEditor); err != nil {
				t.Fatal(err)
			}
		}

		shuffled := slices.Clone(timeline)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		outOfOrder := newUser(t, st, "UTC")
		for len(shuffled) > 0 {
			n := min(len(shuffled), 1+rng.IntN(wire.MaxBulkHeartbeats))
			if _, err := svc.Ingest(ctx, outOfOrder, shuffled[:n], ingest.SourceImport); err != nil {
				t.Fatal(err)
			}
			shuffled = shuffled[n:]
		}

		for _, u := range []uuid.UUID{inOrder, outOfOrder} {
			if n := countRows(t, st, "SELECT count(*) FROM heartbeats WHERE user_id = $1", u); n != len(timeline) {
				t.Fatalf("seed %d: stored %d heartbeats, want %d", seed, n, len(timeline))
			}
		}
		want := expectedMinutes(timeline)
		if got := minutesOf(t, st, inOrder); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: in-order rollups differ from a full recompute", seed)
		}
		if got := minutesOf(t, st, outOfOrder); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: out-of-order rollups differ from in-order ones", seed)
		}
		assertMinuteCap(t, want)
	}
}

func TestIngestConcurrentBatchesForOneUser(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	timeline := randomTimeline(rand.New(rand.NewPCG(3, 3)), base, 400)

	// Interleave so every batch touches every other batch's windows.
	batches := make([][]wire.Heartbeat, 8)
	for i, h := range timeline {
		batches[i%len(batches)] = append(batches[i%len(batches)], h)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(batches))
	for _, b := range batches {
		wg.Go(func() {
			if _, err := svc.Ingest(ctx, user, b, ingest.SourceEditor); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got, want := minutesOf(t, st, user), expectedMinutes(timeline); !reflect.DeepEqual(got, want) {
		t.Fatal("concurrent batches produced different rollups from a full recompute")
	}
}

func TestIngestRollupRules(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	m := func(d time.Duration) time.Time { return base.Add(d) }
	hbs := []wire.Heartbeat{
		beat(m(50*time.Second), "alpha"),  // 30s split 10s / 20s across 09:00 and 09:01
		beat(m(80*time.Second), ""),       // 100s to the next, project Unknown
		beat(m(180*time.Second), "alpha"), // gap 5m: credits nothing
		beat(m(480*time.Second), "alpha"), // last heartbeat: credits nothing
	}
	if _, err := svc.Ingest(ctx, user, hbs, ingest.SourceEditor); err != nil {
		t.Fatal(err)
	}
	row := func(d time.Duration, project string, sec int16) minuteRow {
		return minuteRow{Minute: m(d), Project: project, Language: "Go", Editor: "vscode", Category: "coding", Label: "you", Seconds: sec}
	}
	want := []minuteRow{
		row(0, "alpha", 10),
		row(time.Minute, "Unknown", 40),
		row(time.Minute, "alpha", 20),
		row(2*time.Minute, "Unknown", 60),
	}
	if got := minutesOf(t, st, user); !reflect.DeepEqual(got, want) {
		t.Fatalf("user_minutes =\n%+v\nwant\n%+v", got, want)
	}
}

func TestIngestReportsInvalidItemsWithoutFailingTheBatch(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	neg := -1
	noEntity := beat(base.Add(time.Second), "alpha")
	noEntity.Entity = " "
	future := beat(now.Add(2*time.Hour), "alpha")
	badLine := beat(base.Add(3*time.Second), "alpha")
	badLine.Lineno = &neg
	nul := beat(base.Add(4*time.Second), "al\x00pha")

	res, err := svc.IngestBatch(ctx, user, []wire.Heartbeat{
		beat(base, "alpha"), noEntity, future, badLine, nul,
	}, ingest.SourceEditor)
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 2 {
		t.Fatalf("Inserted = %d, want 2", res.Inserted)
	}
	for i, wantErr := range []bool{false, true, true, true, false} {
		item := res.Items[i]
		if gotErr := item.Err != nil; gotErr != wantErr {
			t.Fatalf("item %d err = %v, want error %v", i, item.Err, wantErr)
		}
		if item.Err != nil && !errors.Is(item.Err, wire.ErrInvalidHeartbeat) {
			t.Fatalf("item %d err = %v, want wire.ErrInvalidHeartbeat", i, item.Err)
		}
		if (item.ID == "") != wantErr {
			t.Fatalf("item %d ID = %q", i, item.ID)
		}
	}
	if got := res.Items[4].Heartbeat.Project; got != "alpha" {
		t.Fatalf("NUL byte not stripped: project %q", got)
	}
}

func TestIngestRejectsUnknownSource(t *testing.T) {
	svc, st, _ := newService(t)
	user := newUser(t, st, "UTC")
	_, err := svc.Ingest(context.Background(), user, []wire.Heartbeat{beat(base, "a")}, "browser")
	if !errors.Is(err, ingest.ErrInvalidSource) {
		t.Fatalf("Ingest() err = %v, want ErrInvalidSource", err)
	}
}

func TestIngestCreatesProjects(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	other := newUser(t, st, "UTC")
	hbs := []wire.Heartbeat{beat(base, "alpha"), beat(base.Add(time.Second), "beta"), beat(base.Add(2*time.Second), " \tbeta\n"), beat(base.Add(3*time.Second), "")}
	if _, err := svc.Ingest(ctx, user, hbs, ingest.SourceEditor); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Pool.Query(ctx, "SELECT name FROM projects WHERE user_id = $1 ORDER BY name", user)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if !reflect.DeepEqual(names, []string{"alpha", "beta"}) {
		t.Fatalf("projects = %v, want [alpha beta]", names)
	}
	if n := countRows(t, st, "SELECT count(*) FROM projects WHERE user_id = $1", other); n != 0 {
		t.Fatalf("other user has %d projects", n)
	}
}

func TestIngestNeverTouchesAnotherUsersRollups(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	alice, bob := newUser(t, st, "UTC"), newUser(t, st, "UTC")
	aliceBeats := randomTimeline(rand.New(rand.NewPCG(5, 5)), base, 100)
	if _, err := svc.Ingest(ctx, alice, aliceBeats, ingest.SourceEditor); err != nil {
		t.Fatal(err)
	}
	before := minutesOf(t, st, alice)

	// Bob's heartbeats interleave Alice's exactly in time, including one
	// identical to hers (same fields, so the same fields_hash and time).
	bobBeats := randomTimeline(rand.New(rand.NewPCG(6, 6)), base, 100)
	bobBeats = append(bobBeats, aliceBeats[10])
	n, err := svc.Ingest(ctx, bob, bobBeats, ingest.SourceEditor)
	if err != nil || n != 101 {
		t.Fatalf("bob Ingest() = %d, %v; want 101 (dedup is per user)", n, err)
	}
	if after := minutesOf(t, st, alice); !reflect.DeepEqual(before, after) {
		t.Fatal("another user's heartbeats changed this user's rollups")
	}
	if got, want := minutesOf(t, st, bob), expectedMinutes(bobBeats); !reflect.DeepEqual(got, want) {
		t.Fatal("bob's rollups include data that isn't his")
	}
}

func TestIngestPublishesChangedRange(t *testing.T) {
	svc, st, pub := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	first, last := base.Add(30*time.Second), base.Add(3*time.Hour)

	if _, err := svc.Ingest(ctx, user, []wire.Heartbeat{beat(last, "a"), beat(first, "a")}, ingest.SourceEditor); err != nil {
		t.Fatal(err)
	}
	want := []published{{UserID: user, From: base.Add(-2 * time.Minute), To: base.Add(3*time.Hour + 3*time.Minute)}}
	if got := pub.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("MinutesChanged calls = %+v, want %+v", got, want)
	}

	// Nothing new: no event.
	invalid := beat(base, "a")
	invalid.Entity = ""
	if _, err := svc.Ingest(ctx, user, []wire.Heartbeat{beat(first, "a"), invalid}, ingest.SourceEditor); err != nil {
		t.Fatal(err)
	}
	if got := pub.Calls(); len(got) != 1 {
		t.Fatalf("MinutesChanged called %d times, want 1", len(got))
	}
}

func TestIngestWithoutPublisher(t *testing.T) {
	_, st, _ := newService(t)
	svc := ingest.New(st, nil, ingest.WithClock(clock))
	user := newUser(t, st, "UTC")
	if n, err := svc.Ingest(context.Background(), user, []wire.Heartbeat{beat(base, "a")}, ingest.SourceAgent); err != nil || n != 1 {
		t.Fatalf("Ingest() = %d, %v", n, err)
	}
}

func BenchmarkIngest100(b *testing.B) {
	svc, st, _ := newService(b)
	ctx := context.Background()
	user := newUser(b, st, "UTC")
	rng := rand.New(rand.NewPCG(9, 9))
	origin := now.Add(-300 * 24 * time.Hour)
	start := origin
	b.ReportAllocs()
	for b.Loop() {
		batch := randomTimeline(rng, start, wire.MaxBulkHeartbeats)
		start = batch[len(batch)-1].Timestamp()
		if start.After(now.Add(-24 * time.Hour)) {
			start = origin // wrap: later batches become duplicates of earlier ones
		}
		if _, err := svc.Ingest(ctx, user, batch, ingest.SourceEditor); err != nil {
			b.Fatal(err)
		}
	}
}

// expectedMinutes is a full recompute of hbs with the pure rollup package.
func expectedMinutes(hbs []wire.Heartbeat) []minuteRow {
	rhbs := make([]rollup.Heartbeat, len(hbs))
	for i, h := range hbs {
		project := h.Project
		rhbs[i] = rollup.Heartbeat{Time: h.Timestamp(), Key: rollup.Key{
			Project: project, Language: h.Language, Editor: h.Editor, Category: h.Category,
		}}
	}
	slices.SortFunc(rhbs, func(a, b rollup.Heartbeat) int { return a.Time.Compare(b.Time) })
	all := []rollup.Range{{Start: rhbs[0].Time.Add(-time.Hour), End: rhbs[len(rhbs)-1].Time.Add(time.Hour)}}
	var out []minuteRow
	for _, r := range rollup.Compute(rhbs, all) {
		out = append(out, minuteRow{
			Minute: r.Minute, Project: r.Key.Project, Language: r.Key.Language, Editor: r.Key.Editor,
			Category: r.Key.Category, Label: "you", Seconds: r.Seconds,
		})
	}
	return out
}

func assertMinuteCap(t *testing.T, rows []minuteRow) {
	t.Helper()
	per := map[time.Time]int{}
	for _, r := range rows {
		per[r.Minute] += int(r.Seconds)
	}
	for m, s := range per {
		if s > 60 {
			t.Fatalf("minute %v holds %d seconds", m, s)
		}
	}
}
