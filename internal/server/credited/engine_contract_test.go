package credited

import (
	"fmt"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// The agent uploads struggle.Compute's minutes and drops whatever the server
// rejects, so any valid engine output that Plan rejects loses honest credit.
// These tests run engine output through Parse and Plan and require every row
// to be accepted with its credit intact.

// engineMinute is an Input for the single minute 10:00 with the person
// typing in the editor throughout and one human write heartbeat per project
// name, a second apart.
func engineMinute(projects ...string) struggle.Input {
	m := at(10, 0)
	in := struggle.Input{Window: struggle.Window{From: m, To: m.Add(time.Minute)}}
	for x := m.Add(-5 * time.Minute); x.Before(m.Add(5 * time.Minute)); x = x.Add(5 * time.Second) {
		in.Activity = append(in.Activity, wire.ActivitySample{Time: x, IdleSeconds: 1, FocusKind: "editor"})
	}
	for i, p := range projects {
		in.Heartbeats = append(in.Heartbeats, struggle.Heartbeat{
			Time: m.Add(time.Duration(i+1) * time.Second), Project: p, Entity: fmt.Sprintf("/f%d.go", i), IsWrite: true,
		})
	}
	return in
}

// uploadEngineOutput sends res the way the agent does, as one batch for a
// device with nothing stored, and returns the credit Plan would store.
func uploadEngineOutput(t *testing.T, res struggle.Result) (stored int) {
	t.Helper()
	raw := make([]RawItem, 0, len(res.Minutes))
	for _, m := range res.Minutes {
		raw = append(raw, RawItem{
			Minute: m.Minute.Format(time.RFC3339), Project: m.Project, Label: string(m.Label),
			Credited: m.Credited, Cuts: m.Cuts, Flags: m.Flags,
		})
	}
	valid, rejected := Parse(raw, now)
	if len(rejected) != 0 {
		t.Fatalf("Parse rejected engine output: %+v", rejected)
	}
	write, rejected := Plan(valid, nil, bounds())
	if len(rejected) != 0 {
		t.Fatalf("Plan rejected engine output: %+v", rejected)
	}
	for _, it := range write {
		stored += it.Credited
	}
	return stored
}

func engineCredit(res struggle.Result) int {
	total := 0
	for _, m := range res.Minutes {
		total += m.Credited
	}
	return total
}

// Two spellings of one project in one minute reach the server as one row.
func TestEngineOutputSurvivesNormalisation(t *testing.T) {
	for _, names := range [][]string{
		{"app", " app "},
		{"app", "app\t", "a\x00pp"},
		{"app", "<<LAST_PROJECT>>"},
	} {
		res := struggle.Compute(engineMinute(names...))
		if got, want := uploadEngineOutput(t, res), engineCredit(res); got != want || want != 60 {
			t.Errorf("projects %q: engine credited %d s, server stored %d s, want 60", names, want, got)
		}
	}
}

// Adversarial many-project minutes: up to 40 projects with uneven counts,
// from heartbeats and AI events, over a few minutes. Plan stores all of the
// engine's credit.
func TestEngineOutputSurvivesManyProjects(t *testing.T) {
	r := uint64(1)
	next := func(n int) int { // xorshift, stable across Go releases
		r ^= r << 13
		r ^= r >> 7
		r ^= r << 17
		return int(r % uint64(n))
	}
	for scenario := range 200 {
		start := at(10, 0)
		in := engineMinute()
		in.Window.To = start.Add(3 * time.Minute)
		for x := start.Add(5 * time.Minute); x.Before(start.Add(8 * time.Minute)); x = x.Add(5 * time.Second) {
			in.Activity = append(in.Activity, wire.ActivitySample{Time: x, IdleSeconds: 1, FocusKind: "editor"})
		}
		projects := 1 + next(40)
		for range 1 + next(150) {
			p := fmt.Sprintf("p%02d", next(projects))
			if next(4) == 0 {
				p = " " + p // a spelling that normalises to the same project
			}
			ts := start.Add(time.Duration(next(180_000)) * time.Millisecond)
			if next(3) == 0 {
				in.AI = append(in.AI, wire.AIEvent{Time: ts, Kind: wire.AIToolStart, Harness: wire.HarnessClaudeCode, SessionID: "s", Project: p})
				continue
			}
			in.Heartbeats = append(in.Heartbeats, struggle.Heartbeat{Time: ts, Project: p, Entity: "/" + p, IsWrite: next(2) == 0, AI: next(5) == 0})
		}
		res := struggle.Compute(in)
		if got, want := uploadEngineOutput(t, res), engineCredit(res); got != want {
			t.Fatalf("scenario %d: engine credited %d s, server stored %d s", scenario, want, got)
		}
		perMinute := map[time.Time]int{}
		for _, m := range res.Minutes {
			perMinute[m.Minute] += m.Credited
		}
		for m, c := range perMinute {
			if c != 60 {
				t.Fatalf("scenario %d: minute %s credited %d s, want 60", scenario, m.Format("15:04"), c)
			}
		}
	}
}
