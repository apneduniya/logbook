package localdb

import (
	"context"
	"slices"
	"testing"

	"github.com/apneduniya/logbook/pkg/wire"
)

func TestUncovered(t *testing.T) {
	run := func(a, b int) span { return span{at(a), at(b)} }
	tests := []struct {
		name string
		runs []span
		want []span
	}{
		{"never up", nil, []span{run(-60, 0)}},
		{"up all along", []span{run(-90, 0)}, nil},
		// Margins: 2 min before a start, 1 min after the last mark.
		{"started inside", []span{run(-40, 0)}, []span{run(-60, -42)}},
		{"stopped inside", []span{run(-90, -20)}, []span{run(-19, 0)}},
		{"quick restart is covered", []span{run(-90, -31), run(-30, 0)}, nil},
		{"stopped 10 minutes", []span{run(-90, -31), run(-20, 0)}, []span{run(-30, -22)}},
		{"two holes", []span{run(-50, -40), run(-20, -10)}, []span{run(-60, -52), run(-39, -22), run(-9, 0)}},
		{"run starting just after the window", []span{run(1, 5)}, []span{run(-60, -1)}},
		{"run long after the window", []span{run(10, 15)}, []span{run(-60, 0)}},
		{"nested runs", []span{run(-90, 0), run(-50, -40)}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := uncovered(tc.runs, at(-60), at(0))
			if len(got) != len(tc.want) {
				t.Fatalf("uncovered = %v, want %v", got, tc.want)
			}
			for i := range got {
				if !got[i].From.Equal(tc.want[i].From) || !got[i].To.Equal(tc.want[i].To) {
					t.Fatalf("uncovered = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A session whose hook events or points fall in a time the agent was
// stopped is left out: its exports may be missing, which would look like
// zero active time. A session entirely inside covered time stays.
func TestActiveTimesLeavesOutSessionsWithLostExports(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	// The agent was stopped from -40 to -30.
	for _, r := range [][2]int{{-120, -41}, {-28, 0}} {
		if err := d.MarkReceiverUp(ctx, at(r[0]), at(r[1])); err != nil {
			t.Fatal(err)
		}
	}
	// A run's latest mark moves forward; an older mark never moves it back.
	if err := d.MarkReceiverUp(ctx, at(-28), at(-27)); err != nil {
		t.Fatal(err)
	}
	evs := []wire.LocalEvent{
		// "spans" had hook events during the stop, but points only after it.
		{Type: "ai", AI: &wire.AIEvent{Time: at(-35), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit, SessionID: "spans"}},
		{Type: "ai", AI: &wire.AIEvent{Time: at(-5), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit, SessionID: "spans"}},
		{Type: "ai", AI: &wire.AIEvent{Time: at(-20), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit, SessionID: "after"}},
	}
	if err := d.InsertEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	pts := []ActiveTimePoint{
		{From: at(-10), To: at(-9), SessionID: "spans", Type: ActiveUser, Secs: 30},
		{From: at(-20), To: at(-19), SessionID: "after", Type: ActiveUser, Secs: 30},
		{From: at(-50), To: at(-49), SessionID: "before", Type: ActiveCLI, Secs: 30},
		{From: at(-35), To: at(-34), SessionID: "during", Type: ActiveCLI, Secs: 30},
	}
	if err := d.InsertActiveTime(ctx, pts); err != nil {
		t.Fatal(err)
	}
	got, err := d.ActiveTimes(ctx, at(-60), at(0))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range got {
		ids = append(ids, a.SessionID)
	}
	if want := []string{"before", "after"}; !slices.Equal(ids, want) {
		t.Fatalf("sessions = %v, want %v", ids, want)
	}
}
