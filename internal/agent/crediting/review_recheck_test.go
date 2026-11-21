// Regression tests from the cross-model re-check of ADR 0017's review fixes: deletion floor, migration probes, minute keys and device replacement.

package crediting

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

func TestRecheckFloorRunnerResumeAndCache(t *testing.T) {
	ctx := context.Background()
	dbNow := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := dbNow.Add(-30*time.Minute + 5*time.Second)
	d, err := localdb.Open(ctx, filepath.Join(t.TempDir(), "agent.db"), localdb.WithEventCap(1), localdb.WithClock(func() time.Time { return dbNow }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	old := wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{Time: dbNow.Add(-4 * time.Hour), FocusKind: "editor"}}
	if err := d.InsertEvents(ctx, []wire.LocalEvent{old}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetMany(ctx, localdb.KV{Key: localdb.KeySealed, Value: "1"}); err != nil {
		t.Fatal(err)
	}
	fresh := old
	copySample := *old.Activity
	copySample.Time = dbNow.Add(-time.Minute)
	fresh.Activity = &copySample
	if err := d.InsertEvents(ctx, []wire.LocalEvent{fresh}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r := New(Options{Store: d, Now: func() time.Time { return now }, Target: func() Target { return Target{Disabled: "test"} }, Compute: func(in struggle.Input) struggle.Result { calls++; return struggle.Compute(in) }})
	r.RunOnce(ctx)
	if calls != 1 {
		t.Fatalf("30-minute rollback stalled: calls=%d", calls)
	}
	now = dbNow.Add(-90*time.Minute + 5*time.Second)
	r.RunOnce(ctx)
	if calls != 1 {
		t.Fatalf("below-floor run computed")
	}
	// The history starts exactly at the floor: computation must resume.
	now = dbNow.Add(-time.Hour + 5*time.Second)
	r.RunOnce(ctx)
	if calls != 2 {
		t.Fatalf("did not resume at floor: calls=%d", calls)
	}
	// Cache generation changes in a snapshot exactly at its history boundary.
	var cached eventCache
	if err := d.View(ctx, func(s *localdb.Snapshot) error { return cached.refresh(ctx, s, dbNow.Add(-3*time.Hour), dbNow) }); err != nil {
		t.Fatal(err)
	}
	dbNow = dbNow.Add(4 * time.Hour)
	if err := d.SetMany(ctx, localdb.KV{Key: localdb.KeySealed, Value: "2"}); err != nil {
		t.Fatal(err)
	}
	copySample.Time = dbNow.Add(-time.Minute)
	fresh.Activity = &copySample
	if err := d.InsertEvents(ctx, []wire.LocalEvent{fresh}); err != nil {
		t.Fatal(err)
	}
	if err := d.View(ctx, func(s *localdb.Snapshot) error {
		if err := cached.refresh(ctx, s, s.Floor, dbNow); err != nil {
			return err
		}
		cold, err := s.Events(ctx, s.Floor, dbNow.Add(futureSlack), 0, s.H)
		if len(cached.events.Activity) != len(cold.Activity) {
			t.Fatalf("cache=%d cold=%d", len(cached.events.Activity), len(cold.Activity))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A first run after a long sleep must still compute normally.
	dbNow = dbNow.Add(100 * 24 * time.Hour)
	now = dbNow.Add(5 * time.Second)
	if err := d.SetMany(ctx, localdb.KV{Key: localdb.KeySealed, Value: "3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	r.RunOnce(ctx)
	if calls != 3 {
		t.Fatalf("long-sleep run stalled: calls=%d", calls)
	}
}
