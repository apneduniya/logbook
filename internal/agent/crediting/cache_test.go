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

// The cached loader returns exactly what a cold read of the window returns,
// in the same order, for random sequences of window moves (a minute on,
// back by minutes, jumps of days either way) and random inserts, many of
// them late (times well before the window's end) or ahead of it.
func TestEventCacheMatchesColdRead(t *testing.T) {
	for seed := range uint64(25) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			ctx := context.Background()
			rng := rand.New(rand.NewPCG(seed, 7))
			base := time.Date(2026, 11, 20, 12, 0, 5, 0, time.UTC)
			now := base
			db, err := localdb.Open(ctx, filepath.Join(t.TempDir(), "agent.db"), localdb.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })

			var c eventCache
			for step := range 80 {
				insertRandom(t, db, rng, now)
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
				s := spanAt(now)
				if err := c.refresh(ctx, db, s.history, s.to); err != nil {
					t.Fatal(err)
				}
				got := c.before(s.to)
				hw, err := db.HighWater(ctx)
				if err != nil {
					t.Fatal(err)
				}
				want, err := db.EventsBetween(ctx, s.history, s.to, &hw)
				if err != nil {
					t.Fatal(err)
				}
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

// A row stored with a time past the cached range (more than futureSlack
// ahead) is not read when it arrives. When the window later moves over it
// one minute at a time, it must be read then: the cache can't rely on ids
// alone for rows it skipped by time.
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
		s := spanAt(now)
		if err := c.refresh(ctx, db, s.history, s.to); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if got := c.before(now); len(got.Activity) != 1 || !got.Activity[0].Time.Equal(ahead) {
		t.Fatalf("cached activity = %+v, want the sample at %s", got.Activity, ahead.Format(time.TimeOnly))
	}
}
