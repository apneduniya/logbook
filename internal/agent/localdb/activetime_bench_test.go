package localdb

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// The crediting pipeline reads a 2-hour window every minute. With a full
// day of three parallel Claude Code sessions (user and cli points every
// minute) and a busy day of hook events, the read must stay a small part of
// the 50 ms budget.
func BenchmarkActiveTimes2h(b *testing.B) {
	ctx := context.Background()
	path := filepath.Join(b.TempDir(), "logbook.db")
	d, err := Open(ctx, path, WithClock(func() time.Time { return testNow }))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = d.Close() })
	evs := make([]wire.LocalEvent, 0, 20_000)
	for i := range 20_000 {
		ev := wire.AIEvent{Time: testNow.Add(-time.Duration(i) * 4 * time.Second), Harness: wire.HarnessClaudeCode,
			Kind: wire.AIToolStart, SessionID: fmt.Sprintf("s%d", i%3), Cwd: "/w/app"}
		evs = append(evs, wire.LocalEvent{Type: "ai", AI: &ev})
	}
	if err := d.InsertEvents(ctx, evs); err != nil {
		b.Fatal(err)
	}
	var pts []ActiveTimePoint
	for m := range 24 * 60 {
		for s := range 3 {
			for _, typ := range []string{ActiveUser, ActiveCLI} {
				pts = append(pts, ActiveTimePoint{From: at(-m - 1), To: at(-m), SessionID: fmt.Sprintf("s%d", s), Type: typ, Secs: 20})
			}
		}
	}
	if err := d.InsertActiveTime(ctx, pts); err != nil {
		b.Fatal(err)
	}
	// The agent restarted 45 minutes ago (a gap its margins cover).
	if err := d.MarkReceiverUp(ctx, at(-24*60), at(-46)); err != nil {
		b.Fatal(err)
	}
	if err := d.MarkReceiverUp(ctx, at(-45), at(0)); err != nil {
		b.Fatal(err)
	}
	bench := func(b *testing.B, want int) {
		for b.Loop() {
			got, err := d.ActiveTimes(ctx, at(-120), at(0))
			if err != nil || len(got) != want {
				b.Fatalf("ActiveTimes = %d, %v", len(got), err)
			}
		}
	}
	b.Run("covered", func(b *testing.B) { bench(b, 120*6) })
	// The agent was stopped for 10 minutes: every session's span is checked
	// against its hook events, and all three are left out.
	if err := d.MarkReceiverUp(ctx, at(-30), at(0)); err != nil {
		b.Fatal(err)
	}
	if _, err := d.sql.ExecContext(ctx, "DELETE FROM otlp_uptime WHERE from_ms = ?", at(-45).UnixMilli()); err != nil {
		b.Fatal(err)
	}
	b.Run("agent_stopped", func(b *testing.B) { bench(b, 0) })
}
