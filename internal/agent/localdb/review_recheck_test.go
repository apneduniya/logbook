// Regression tests from the cross-model re-check of ADR 0017's review fixes: deletion floor, migration probes, minute keys and device replacement.

package localdb

import (
	"context"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

func TestRecheckPruneFloorAndActiveOverlap(t *testing.T) {
	ctx := context.Background()
	clock := testNow
	d, _ := openTest(t, WithClock(func() time.Time { return clock }))
	cut := clock.Add(-DefaultRetention)
	old := cut.Add(-time.Minute)
	if err := d.InsertEvents(ctx, []wire.LocalEvent{activity(old)}); err != nil {
		t.Fatal(err)
	}
	h := hb(1)
	h.Time = float64(old.Unix())
	if err := d.Enqueue(ctx, []wire.Heartbeat{h}, false); err != nil {
		t.Fatal(err)
	}
	pts := []ActiveTimePoint{
		{From: old.Add(-time.Minute), To: old, SessionID: "gone", Type: ActiveUser, Secs: 30},
		{From: old, To: cut.Add(time.Minute), SessionID: "overlap", Type: ActiveUser, Secs: 60},
		{From: old, To: cut, SessionID: "boundary", Type: ActiveUser, Secs: 60},
	}
	if err := d.InsertActiveTime(ctx, pts); err != nil {
		t.Fatal(err)
	}
	seal(t, d, high(ctx, t, d))
	pr, err := d.Prune(ctx)
	if err != nil || pr.ActivitySamples != 1 || pr.ActiveTime != 1 {
		t.Fatalf("raw prune=%+v %v", pr, err)
	}
	cp, err := d.PruneCrediting(ctx)
	if err != nil || cp.Heartbeats != 1 {
		t.Fatalf("credit prune=%+v %v", cp, err)
	}
	if got := floor(ctx, t, d); !got.Equal(cut) {
		t.Fatalf("floor=%s want=%s", got, cut)
	}
	if n := countRows(t, d.sql, "active_time"); n != 2 {
		t.Fatalf("overlapping active rows=%d", n)
	}
	// A backward jump and a second prune must not lower the floor.
	clock = clock.Add(-time.Hour)
	if _, err := d.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if got := floor(ctx, t, d); !got.Equal(cut) {
		t.Fatalf("floor moved backwards: %s", got)
	}
}

func TestRecheckHeartbeatQueueTrimFloor(t *testing.T) {
	ctx := context.Background()
	clock := testNow
	d, _ := openTest(t, WithEventCap(1), WithClock(func() time.Time { return clock }))
	h := hb(1)
	h.Time = float64(clock.Add(-4 * time.Hour).Unix())
	if err := d.Enqueue(ctx, []wire.Heartbeat{h}, true); err != nil {
		t.Fatal(err)
	}
	seal(t, d, high(ctx, t, d))
	h.Time = float64(clock.Add(-time.Minute).Unix())
	if err := d.Enqueue(ctx, []wire.Heartbeat{h}, true); err != nil {
		t.Fatal(err)
	}
	if got := floor(ctx, t, d); !got.Equal(clock.Add(-CapHorizon)) {
		t.Fatalf("floor=%s", got)
	}
	if n := countRows(t, d.sql, "heartbeats"); n != 1 {
		t.Fatalf("rows=%d", n)
	}
	g, err := d.EventGeneration(ctx)
	if err != nil || g != 1 {
		t.Fatalf("generation=%d %v", g, err)
	}
	// Recent sealed rows still survive even when the queue path exceeds cap.
	seal(t, d, high(ctx, t, d))
	h.Time = float64(clock.Unix())
	if err := d.Enqueue(ctx, []wire.Heartbeat{h}, false); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d.sql, "heartbeats"); n != 2 {
		t.Fatalf("recent rows trimmed=%d", n)
	}
}

func TestRecheckRepoPruneCurrentSnapshot(t *testing.T) {
	ctx := context.Background()
	clock := testNow
	d, _ := openTest(t, WithClock(func() time.Time { return clock }))
	old := testNow.Add(-DefaultRetention - time.Hour)
	if err := d.RecordRepos(ctx, old, []struggle.Repo{{Path: "/src/app", Project: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRepos(ctx, old.Add(time.Minute), []struggle.Repo{{Path: "/src/app", Project: "b"}}); err != nil {
		t.Fatal(err)
	}
	seal(t, d, high(ctx, t, d))
	h := high(ctx, t, d)
	if _, err := d.PruneCrediting(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d.sql, "repo_map"); n != 1 {
		t.Fatalf("repo rows=%d", n)
	}
	clock = old.Add(-time.Hour)
	if err := d.View(ctx, func(s *Snapshot) error {
		repos, err := s.Repos(ctx, h)
		if len(repos) != 1 || repos[0].Project != "b" {
			t.Fatalf("current snapshot mapping=%+v", repos)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := floor(ctx, t, d); !got.Equal(time.UnixMilli(0)) {
		t.Fatalf("repo-only prune unnecessarily moved floor=%s", got)
	}
}
