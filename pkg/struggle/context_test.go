package struggle

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// A sample long after a prompt can reveal input just before it. Once the
// prompt's minute is final, that sample must not change its
// automated_session flag: the agent never saw it.
func TestLateSampleDoesNotChangeAutomatedSession(t *testing.T) {
	noon := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	full := Input{Window: Window{From: noon, To: noon.Add(10 * time.Minute)}}
	// The last input was at 11:50; the sensor then stalls after 12:00:00.
	for x := noon.Add(-15 * time.Minute); !x.After(noon); x = x.Add(5 * time.Second) {
		idle := max(0, x.Sub(noon.Add(-10*time.Minute)).Seconds())
		full.Activity = append(full.Activity, wire.ActivitySample{Time: x, IdleSeconds: idle, FocusKind: "terminal"})
	}
	full.AI = []wire.AIEvent{
		{Time: noon.Add(time.Second), Kind: wire.AIPromptSubmit, Harness: wire.HarnessClaudeCode, SessionID: "s", Project: "app"},
		{Time: noon.Add(5 * time.Minute), Kind: wire.AITurnEnd, Harness: wire.HarnessClaudeCode, SessionID: "s", Project: "app"},
	}
	// An hour later the sensor reports the input it missed at 12:00:01.
	full.Activity = append(full.Activity, wire.ActivitySample{Time: noon.Add(time.Hour), IdleSeconds: 3599, FocusKind: "terminal"})

	agent := sliceInput(full, noon.Add(-ContextBefore), noon.Add(Lookahead))
	agent.Window = Window{From: noon, To: noon.Add(time.Minute)}
	agent.DataEnd = noon.Add(Lookahead)

	flagged := func(res Result) bool {
		for _, m := range res.Minutes {
			if m.Minute.Equal(noon) {
				return slices.Contains(m.Flags, FlagAutomatedSession)
			}
		}
		return false
	}
	if !flagged(Compute(agent)) {
		t.Fatal("the agent's final view of 12:00 is not flagged automated_session")
	}
	if !flagged(Compute(full)) {
		t.Fatal("a sample an hour later removed the automated_session flag from final minute 12:00")
	}
}

// TestBoundedContext is the finality contract (ADR 0012) as a property: for
// random scenarios, every minute's result computed from only the events in
// [minute-ContextBefore, minute+Lookahead) equals its result computed from
// all events, apart from the window-relative flags. The scenarios include
// sensor stalls, late samples that reveal old input, and events far before
// and after the minute.
func TestBoundedContext(t *testing.T) {
	seeds := 40
	if testing.Short() {
		seeds = 10
	}
	for seed := range seeds {
		full := randomScenario(uint64(seed + 1))
		want := Compute(full)
		for m := full.Window.From; m.Before(full.Window.To); m = m.Add(time.Minute) {
			in := sliceInput(full, m.Add(-ContextBefore), m.Add(Lookahead))
			in.Window = Window{From: m, To: m.Add(time.Minute)}
			in.DataEnd = m.Add(Lookahead)
			compareMinutes(t, fmt.Sprintf("seed %d, bounded context", seed+1), Compute(in), want, m, m.Add(time.Minute))
		}
	}
}

// randomScenario is three hours of one device's events, with the window
// being the middle hour. Every kind of event is drawn at random, with
// pauses, stalls and late observations.
func randomScenario(seed uint64) Input {
	r := rng(seed)
	pick := func(n int) int { return int(r.float() * float64(n)) }
	chance := func(p float64) bool { return r.float() < p }
	start := t0
	end := start.Add(3 * time.Hour)
	in := Input{
		Window: Window{From: start.Add(time.Hour), To: start.Add(2 * time.Hour)},
		Repos:  []Repo{{Path: "/w/app", Project: "app"}, {Path: "/w/lib", Project: "lib"}},
	}
	projects := []string{"app", "lib", " app", "web", ""}
	focus := []string{"editor", "terminal", "ai", "browser", "other", "unknown"}

	// Input instants in bursts, with pauses.
	var inputs []time.Time
	for x := start; x.Before(end); {
		if chance(0.3) {
			x = x.Add(secs(r.between(60, 1200))) // a pause
			continue
		}
		burstEnd := x.Add(secs(r.between(10, 600)))
		for ; x.Before(burstEnd); x = x.Add(secs(r.between(0.2, 8))) {
			inputs = append(inputs, x)
		}
	}
	// Samples every 5 s, with stalls; a stall may end with a sample that
	// reveals input long ago.
	kind := focus[0]
	for x := start; x.Before(end); x = x.Add(5 * time.Second) {
		if chance(0.002) {
			x = x.Add(secs(r.between(60, 4000)))
		}
		if chance(0.02) {
			kind = focus[pick(len(focus))]
		}
		i, _ := slices.BinarySearchFunc(inputs, x, time.Time.Compare)
		idle := 3600.0
		if i > 0 {
			idle = x.Sub(inputs[i-1]).Seconds()
		}
		in.Activity = append(in.Activity, wire.ActivitySample{Time: x, IdleSeconds: idle, FocusKind: kind})
	}
	// Heartbeats, human and AI, in a few files.
	line := 1
	for x := start; x.Before(end); x = x.Add(secs(r.between(5, 240))) {
		f := pick(4)
		line += pick(3)
		l := line
		in.Heartbeats = append(in.Heartbeats, Heartbeat{
			Time: x, Project: projects[pick(len(projects))], Entity: fmt.Sprintf("/w/%s/f%d.go", []string{"app", "lib", "x", "y"}[f], f),
			IsWrite: chance(0.2), Lineno: &l, AI: chance(0.3),
		})
	}
	// AI sessions: prompts (some short, some repeated), tool calls,
	// permission asks and answers, turn ends, timeouts.
	for s := range 3 {
		session := fmt.Sprint("s", s)
		cwd := []string{"/w/app", "/w/lib", "/w/other"}[s]
		ev := func(x time.Time, k wire.AIEventKind) wire.AIEvent {
			return wire.AIEvent{Time: x, Kind: k, Harness: wire.HarnessClaudeCode, SessionID: session, Cwd: cwd, Project: projects[pick(len(projects))]}
		}
		for x := start.Add(secs(r.between(0, 1800))); x.Before(end); {
			if chance(0.2) {
				in.AI = append(in.AI, ev(x, wire.AIToolStart))
			} else {
				e := ev(x, wire.AIPromptSubmit)
				e.PromptChars = []int{0, 3, 10, 40, 200}[pick(5)]
				e.PromptHash = []string{"", "h1", "h2", "h3"}[pick(4)]
				in.AI = append(in.AI, e)
			}
			turnEnd := x.Add(secs(r.between(5, 1800)))
			for x = x.Add(secs(r.between(1, 30))); x.Before(turnEnd); x = x.Add(secs(r.between(1, 400))) {
				switch {
				case chance(0.1):
					in.AI = append(in.AI, ev(x, wire.AIPermissionAsk))
				case chance(0.05):
					a := ev(x, wire.AIPermissionAsk)
					a.Decision = "allow"
					in.AI = append(in.AI, a)
				default:
					in.AI = append(in.AI, ev(x, wire.AIToolStart), ev(x.Add(time.Second), wire.AIToolEnd))
				}
			}
			if chance(0.8) {
				in.AI = append(in.AI, ev(turnEnd, wire.AITurnEnd))
			}
			x = turnEnd.Add(secs([]float64{r.between(1, 15), r.between(15, 300), r.between(300, 3000)}[pick(3)]))
		}
	}
	// Late observations: a sample long after a prompt that reports input
	// just around it, as a sensor catching up after a stall would.
	for _, e := range in.AI {
		if e.Kind == wire.AIPromptSubmit && chance(0.3) {
			input := e.Time.Add(secs(r.between(-3, 3)))
			late := e.Time.Add(secs(r.between(60, 7200)))
			in.Activity = append(in.Activity, wire.ActivitySample{Time: late, IdleSeconds: late.Sub(input).Seconds(), FocusKind: "terminal"})
		}
	}
	return in
}

// A permission wait whose asks repeat for longer than ContextBefore, then an
// answer: the answer (a human signal, so browsing counts) and the busy time
// after it must not depend on seeing the turn's prompt over an hour earlier.
func TestLongPermissionWaitIsBounded(t *testing.T) {
	full := Input{Window: Window{From: at(80 * time.Minute), To: at(100 * time.Minute)}}
	for x := t0; x.Before(at(130 * time.Minute)); x = x.Add(5 * time.Second) {
		full.Activity = append(full.Activity, wire.ActivitySample{Time: x, IdleSeconds: 1, FocusKind: "browser"})
	}
	ev := func(d time.Duration, k wire.AIEventKind) wire.AIEvent {
		return wire.AIEvent{Time: at(d), Kind: k, Harness: wire.HarnessClaudeCode, SessionID: "s", Project: "app"}
	}
	full.AI = append(full.AI, ev(0, wire.AIPromptSubmit))
	for d := time.Minute; d < 85*time.Minute; d += 5 * time.Minute {
		full.AI = append(full.AI, ev(d, wire.AIPermissionAsk))
	}
	full.AI = append(full.AI, ev(85*time.Minute, wire.AIToolEnd), ev(90*time.Minute, wire.AIToolStart))
	want := Compute(full)
	for m := full.Window.From; m.Before(full.Window.To); m = m.Add(time.Minute) {
		in := sliceInput(full, m.Add(-ContextBefore), m.Add(Lookahead))
		in.Window = Window{From: m, To: m.Add(time.Minute)}
		in.DataEnd = m.Add(Lookahead)
		compareMinutes(t, "bounded context", Compute(in), want, m, m.Add(time.Minute))
	}
}
