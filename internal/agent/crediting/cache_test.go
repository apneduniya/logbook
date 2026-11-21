package crediting

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/wire"
)

// sealAll marks every row the test will store as sealed, so retention may
// delete them (it never deletes unsealed rows).
func sealAll(t *testing.T, db *localdb.DB) {
	t.Helper()
	if err := db.SetMany(context.Background(), localdb.KV{Key: localdb.KeySealed, Value: "1000000000"}); err != nil {
		t.Fatal(err)
	}
}

// refreshed refreshes c for span s in one snapshot of db, as a run does,
// and returns the cached window and a cold read of the same snapshot.
func refreshed(t *testing.T, db *localdb.DB, c *eventCache, s span) (got, want localdb.Events) {
	t.Helper()
	ctx := context.Background()
	err := db.View(ctx, func(snap *localdb.Snapshot) error {
		if err := c.refresh(ctx, snap, s.history, s.to); err != nil {
			return err
		}
		got = c.before(s.to)
		var err error
		want, err = snap.Events(ctx, s.history, s.to, 0, snap.H)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, want
}

// The cached loader returns exactly what a cold read of the same snapshot
// returns, in the same order, for random sequences of window moves (a
// minute on, back by minutes, jumps of days either way) and random
// inserts, many of them late (times well before the window's end) or ahead
// of it, while sealed rows are pruned by age and trimmed by a small row
// cap.
func TestEventCacheMatchesColdRead(t *testing.T) {
	for seed := range uint64(25) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			ctx := context.Background()
			rng := rand.New(rand.NewPCG(seed, 7))
			base := time.Date(2026, 11, 20, 12, 0, 5, 0, time.UTC)
			now := base
			db, err := localdb.Open(ctx, filepath.Join(t.TempDir(), "agent.db"), localdb.WithClock(func() time.Time { return now }),
				localdb.WithEventCap(int64(10+rng.IntN(40))), localdb.WithRetention(time.Duration(1+rng.IntN(6))*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			sealAll(t, db)

			var c eventCache
			for step := range 80 {
				insertRandom(t, db, rng, now)
				if rng.IntN(5) == 0 {
					if _, err := db.Prune(ctx); err != nil {
						t.Fatal(err)
					}
					if _, err := db.PruneCrediting(ctx); err != nil {
						t.Fatal(err)
					}
				}
				switch k := rng.IntN(10); {
				case k < 5:
					now = now.Add(time.Minute)
				case k < 7:
					now = now.Add(-time.Duration(1+rng.IntN(120)) * time.Minute)
				case k < 8:
					now = now.Add(time.Duration(1+rng.IntN(5)) * 24 * time.Hour)
				case k < 9:
					now = now.Add(-time.Duration(1+rng.IntN(5)) * 24 * time.Hour)
				default:
					now = now.Add(time.Duration(30+rng.IntN(120)) * time.Minute)
				}
				got, want := refreshed(t, db, &c, spanAt(now))
				if msg := diffEvents(got, want); msg != "" {
					t.Fatalf("step %d at %s: cached window differs from a cold read: %s",
						step, now.Format(time.DateTime), msg)
				}
			}
		})
	}
}

// insertRandom stores a few events of each kind at random times around now,
// some sharing a millisecond, and heartbeats with sub-millisecond times.
func insertRandom(t *testing.T, db *localdb.DB, rng *rand.Rand, now time.Time) {
	t.Helper()
	ctx := context.Background()
	at := func() time.Time {
		switch rng.IntN(4) {
		case 0: // late: anywhere in the last three hours
			return now.Add(-time.Duration(rng.IntN(3*3600*1000)) * time.Millisecond)
		case 1: // ahead of the window, within and past futureSlack
			return now.Add(time.Duration(rng.IntN(2*3600*1000)) * time.Millisecond)
		case 2: // a few days either way
			return now.Add(time.Duration(rng.IntN(10*24)-5*24) * time.Hour)
		}
		return now.Add(-time.Duration(rng.IntN(60)) * time.Second).Truncate(time.Second) // shared times
	}
	var evs []wire.LocalEvent
	var hbs []wire.Heartbeat
	for range rng.IntN(6) {
		switch rng.IntN(3) {
		case 0:
			evs = append(evs, wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{
				Time: at().Truncate(time.Millisecond), IdleSeconds: float64(rng.IntN(100)), FocusKind: "editor"}})
		case 1:
			evs = append(evs, wire.LocalEvent{Type: "ai", AI: &wire.AIEvent{
				Time: at().Truncate(time.Millisecond), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit,
				SessionID: fmt.Sprint(rng.IntN(1000))}})
		default:
			ts := at()
			sec := float64(ts.UnixMilli())/1000 + float64(rng.IntN(1000))/1e6
			hbs = append(hbs, wire.Heartbeat{Entity: fmt.Sprintf("/src/p/%d.go", rng.IntN(1e6)), Project: "p", Time: sec})
		}
	}
	if len(evs) > 0 {
		if err := db.InsertEvents(ctx, evs); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Enqueue(ctx, hbs, false); err != nil {
		t.Fatal(err)
	}
}

// diffEvents describes how got and want differ ("" if they don't). Empty
// and nil slices are the same.
func diffEvents(got, want localdb.Events) string {
	same := func(a, b any) bool {
		va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
		return (va.Len() == 0 && vb.Len() == 0) || reflect.DeepEqual(a, b)
	}
	switch {
	case !same(got.Activity, want.Activity):
		return fmt.Sprintf("activity: %d events, want %d", len(got.Activity), len(want.Activity))
	case !same(got.AI, want.AI):
		return fmt.Sprintf("ai: %d events, want %d", len(got.AI), len(want.AI))
	case !same(got.Heartbeats, want.Heartbeats):
		return fmt.Sprintf("heartbeats: %d events, want %d", len(got.Heartbeats), len(want.Heartbeats))
	case !same(got.Git, want.Git):
		return fmt.Sprintf("git: %d events, want %d", len(got.Git), len(want.Git))
	}
	return ""
}

// With a cap of two rows, a third sample trims the oldest (sealed) one. The
// cache must drop it too: a presence sample kept in memory but gone from
// the table would credit a minute (60 s) that a cold read, or a replay,
// leaves idle. The deletion generation it reads comes from the same
// snapshot as the rows.
func TestEventCacheDropsTrimmedRows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 5, 0, time.UTC)
	// The row cap never trims rows a run's window still covers
	// (localdb.CapHorizon), so the database's clock runs ahead here to
	// trim inside the cached window: the cache must still notice deletions
	// by the generation, whatever deletes them.
	dbNow := now.Add(localdb.CapHorizon + time.Hour)
	db, err := localdb.Open(ctx, filepath.Join(t.TempDir(), "agent.db"),
		localdb.WithClock(func() time.Time { return dbNow }), localdb.WithEventCap(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sealAll(t, db)
	store := func(at time.Time) {
		t.Helper()
		if err := db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity",
			Activity: &wire.ActivitySample{Time: at, IdleSeconds: 1, FocusKind: "editor"}}}); err != nil {
			t.Fatal(err)
		}
	}
	store(now.Add(-10 * time.Minute))
	store(now.Add(-5 * time.Minute))
	var c eventCache
	s := spanAt(now)
	refreshed(t, db, &c, s)
	store(now.Add(-2 * time.Minute)) // trims the sample at -10
	got, want := refreshed(t, db, &c, s)
	if len(got.Activity) != 2 || !got.Activity[0].Event.Time.Equal(now.Add(-5*time.Minute)) || diffEvents(got, want) != "" {
		t.Fatalf("cache holds %d samples (first %v), want the 2 the table keeps", len(got.Activity), got.Activity)
	}
}

// A row stored with a time past the cached range (more than futureSlack
// ahead) is not read when it arrives. When the window later moves over it
// one minute at a time, it must be read then: the cache can't rely on
// sequence numbers alone for rows it skipped by time.
func TestEventCacheRowAheadOfWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 5, 0, time.UTC)
	db, err := localdb.Open(ctx, filepath.Join(t.TempDir(), "agent.db"), localdb.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ahead := now.Add(90 * time.Minute).Truncate(time.Minute)
	if err := db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity",
		Activity: &wire.ActivitySample{Time: ahead, IdleSeconds: 1, FocusKind: "editor"}}}); err != nil {
		t.Fatal(err)
	}
	var c eventCache
	for range 100 {
		refreshed(t, db, &c, spanAt(now))
		now = now.Add(time.Minute)
	}
	if got := c.before(now); len(got.Activity) != 1 || !got.Activity[0].Event.Time.Equal(ahead) {
		t.Fatalf("cached activity = %+v, want the sample at %s", got.Activity, ahead.Format(time.TimeOnly))
	}
}
