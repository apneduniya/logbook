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
	for b.Loop() {
		got, err := d.ActiveTimes(ctx, at(-120), at(0))
		if err != nil || len(got) != 120*6 {
			b.Fatalf("ActiveTimes = %d, %v", len(got), err)
		}
	}
}
