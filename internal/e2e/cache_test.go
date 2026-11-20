package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// An edit at 12:00, then the clock jumps five days ahead and back. The
// minutes after the edit must still get the thinking grace from it: the
// in-memory window must hold the 12:00 events again, not just what arrived
// after the jump.
func TestClockJumpKeepsHistory(t *testing.T) {
	r := newRig(t, minute.Add(time.Minute+5*time.Second))
	r.present(minute.Add(-5*time.Minute), minute.Add(10*time.Minute))
	r.edit(minute.Add(5*time.Second), "app")
	r.run()

	ahead := 5 * 24 * time.Hour
	r.with(func(r *rig) {
		r.now = minute.Add(2*time.Minute + 5*time.Second + ahead)
		r.serverOffset = -ahead
	})
	r.runAny()

	r.with(func(r *rig) {
		r.now = minute.Add(5*time.Minute + 5*time.Second)
		r.serverOffset = 0
	})
	r.run()
	got, total := r.serverRows(minute.Add(2 * time.Minute))
	if total != 60 || !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "app ") }) {
		t.Fatalf("12:02 holds %v (%d s), want 60 s of thinking grace for app", got, total)
	}
	r.wantServerMatchesEngine(minute.Add(2 * time.Minute))
}
