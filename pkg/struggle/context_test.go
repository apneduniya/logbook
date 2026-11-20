package struggle

import (
	"flag"
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

var boundedSeeds = flag.Int("seeds", 40, "random scenarios for TestBoundedContext")

// TestBoundedContext is the finality contract (ADR 0012) as a property: for
// random scenarios, every minute's result computed from only the events in
// [minute-ContextBefore, minute+Lookahead) equals its result computed from
// all events, apart from the window-relative flags. The scenarios include
// sensor stalls, late samples that reveal old input, and events far before
// and after the minute. Slices hold only the events that had arrived by
// their end (sliceInput), so an event stamped before it arrived, as Cursor's
// hooks could produce, must not change a minute that was final by then. The
// agent's own replay, minute by minute, is checked on every scenario too.
func TestBoundedContext(t *testing.T) {
	seeds := *boundedSeeds
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
		t.Run(fmt.Sprintf("seed %d agent replay", seed+1), func(t *testing.T) { checkAgentReplay(t, full) })
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
	// permission asks and answers, turn ends, timeouts, session starts
	// mid-turn (Claude Code's compaction). Sessions 3 to 5 have the shapes
	// of other harnesses' hooks (docs/agent/harness.md): Copilot's tool ends
	// with no tool starts, Cursor's tool calls reported only when they end
	// (cursorToolCall), some after more than Lookahead, OpenCode's replies as
	// decided asks. Their turns can run for
	// over an hour, so the prompt is often outside a minute's context.
	harnesses := []wire.Harness{wire.HarnessClaudeCode, wire.HarnessClaudeCode, wire.HarnessClaudeCode,
		wire.HarnessCopilot, wire.HarnessCursor, wire.HarnessOpenCode}
	for s, h := range harnesses {
		session := fmt.Sprint("s", s)
		cwd := []string{"/w/app", "/w/lib", "/w/other"}[s%3]
		ev := func(x time.Time, k wire.AIEventKind) wire.AIEvent {
			return wire.AIEvent{Time: x, Kind: k, Harness: h, SessionID: session, Cwd: cwd, Project: projects[pick(len(projects))]}
		}
		longest := 1800.0
		if s >= 3 {
			longest = 6000
		}
		for x := start.Add(secs(r.between(0, 1800))); x.Before(end); {
			if chance(0.2) && h != wire.HarnessCopilot {
				in.AI = append(in.AI, ev(x, wire.AIToolStart))
			} else {
				e := ev(x, wire.AIPromptSubmit)
				e.PromptChars = []int{0, 3, 10, 40, 200}[pick(5)]
				e.PromptHash = []string{"", "h1", "h2", "h3"}[pick(4)]
				in.AI = append(in.AI, e)
			}
			turnEnd := x.Add(secs(r.between(5, longest)))
			for x = x.Add(secs(r.between(1, 30))); x.Before(turnEnd); x = x.Add(secs(r.between(1, 400))) {
				switch {
				case chance(0.03):
					in.AI = append(in.AI, ev(x, wire.AISessionStart))
				case chance(0.1) && h != wire.HarnessCursor:
					in.AI = append(in.AI, ev(x, wire.AIPermissionAsk))
				case chance(0.05) && (h == wire.HarnessClaudeCode || h == wire.HarnessOpenCode):
					a := ev(x, wire.AIPermissionAsk)
					a.Decision = "allow"
					in.AI = append(in.AI, a)
				case h == wire.HarnessCopilot:
					in.AI = append(in.AI, ev(x, wire.AIToolEnd))
				case h == wire.HarnessCursor:
					// Some calls run for longer than Lookahead: Cursor reports
					// them only when they end.
					took := secs(r.between(0, 1200))
					if chance(0.15) {
						took = secs(r.between(1200, 3600))
					}
					in.AI = append(in.AI, cursorToolCall(ev, x.Add(-took), x)...)
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
	checkBounded(t, full)
}

// checkBounded computes every minute of full's window from only the events in
// [minute-ContextBefore, minute+Lookahead), compares it with the full run,
// and returns the full run.
func checkBounded(t *testing.T, full Input) Result {
	t.Helper()
	want := Compute(full)
	for m := full.Window.From; m.Before(full.Window.To); m = m.Add(time.Minute) {
		in := sliceInput(full, m.Add(-ContextBefore), m.Add(Lookahead))
		in.Window = Window{From: m, To: m.Add(time.Minute)}
		in.DataEnd = m.Add(Lookahead)
		compareMinutes(t, "bounded context", Compute(in), want, m, m.Add(time.Minute))
	}
	return want
}

// labelAt returns the label of minute m in res; a minute not emitted is idle.
func labelAt(res Result, m time.Time) Label {
	for _, row := range res.Minutes {
		if row.Minute.Equal(m) {
			return row.Label
		}
	}
	return LabelIdle
}

// midTurnStream is one session's prompt at t0 followed by an event of kind
// every 5 minutes up to +90 minutes, with nobody at the machine.
func midTurnStream(h wire.Harness, kind wire.AIEventKind, decision string) Input {
	full := Input{Window: Window{From: at(70 * time.Minute), To: at(90 * time.Minute)}}
	for x := t0; x.Before(at(120 * time.Minute)); x = x.Add(5 * time.Second) {
		full.Activity = append(full.Activity, wire.ActivitySample{Time: x, IdleSeconds: 3600, FocusKind: "terminal"})
	}
	full.AI = append(full.AI, wire.AIEvent{Time: t0, Kind: wire.AIPromptSubmit, Harness: h, SessionID: "s", Project: "app", PromptChars: 80})
	for d := 5 * time.Minute; d <= 90*time.Minute; d += 5 * time.Minute {
		full.AI = append(full.AI, wire.AIEvent{Time: at(d), Kind: kind, Harness: h, SessionID: "s", Project: "app", Decision: decision})
	}
	return full
}

// GitHub Copilot's hooks emit tool ends but no tool starts. A prompt followed
// by a tool end every 5 minutes is one running turn. At +80 minutes the
// prompt is outside ContextBefore, and the minute must still be agent time.
func TestToolEndOnlyTurnIsBounded(t *testing.T) {
	want := checkBounded(t, midTurnStream(wire.HarnessCopilot, wire.AIToolEnd, ""))
	if got := labelAt(want, at(80*time.Minute)); got != LabelAgent {
		t.Fatalf("minute +80 of a running Copilot turn is %s, want agent", got)
	}
}

// OpenCode reports the person's reply to a permission ask as a
// permission_ask with a Decision. A stream of replies is a running turn too.
func TestDecidedAskOnlyTurnIsBounded(t *testing.T) {
	want := checkBounded(t, midTurnStream(wire.HarnessOpenCode, wire.AIPermissionAsk, "allow"))
	if got := labelAt(want, at(80*time.Minute)); got != LabelAgent {
		t.Fatalf("minute +80 of a running OpenCode turn is %s, want agent", got)
	}
}

// session_start isn't agent work (Claude Code also sends it on resume and
// compaction): a run of them after a prompt doesn't keep the prompt's turn
// running, so minute +80 doesn't depend on a prompt 80 minutes back.
func TestSessionStartDoesNotKeepTurnOpen(t *testing.T) {
	want := checkBounded(t, midTurnStream(wire.HarnessClaudeCode, wire.AISessionStart, ""))
	if got := labelAt(want, at(80*time.Minute)); got != LabelIdle {
		t.Fatalf("minute +80 after only session starts is %s, want idle", got)
	}
}

// Cursor reports a tool call only when it ends. The same long prompt at −4,
// −2 and 0 minutes, then a tool call from +8 to +48: when minute +2 becomes
// final (by +27) Cursor hasn't reported the call, so the third prompt's turn
// ended at its prompt and +2 is the person's minute with full credit. When
// the call arrives at +48 it must not extend that turn back through +8 and
// cut +2 as a loop: agent and server would disagree, and an honest minute
// would lose its credit after the fact.
func TestLateCursorToolCallKeepsFinalMinutes(t *testing.T) {
	m := time.Minute
	full := Input{Window: Window{From: at(-5 * m), To: at(30 * m)}, DataEnd: at(90 * m)}
	for x := at(-20 * m); x.Before(at(90 * m)); x = x.Add(5 * time.Second) {
		full.Activity = append(full.Activity, wire.ActivitySample{Time: x, IdleSeconds: 1, FocusKind: "terminal"})
	}
	ev := func(x time.Time, k wire.AIEventKind) wire.AIEvent {
		return wire.AIEvent{Time: x, Kind: k, Harness: wire.HarnessCursor, SessionID: "s", Project: "app"}
	}
	for _, d := range []time.Duration{-4 * m, -2 * m, 0} {
		p := ev(at(d), wire.AIPromptSubmit)
		p.PromptChars, p.PromptHash = 80, "same"
		full.AI = append(full.AI, p)
	}
	full.AI = append(full.AI, cursorToolCall(ev, at(8*m), at(48*m))...)
	full.AI = append(full.AI, ev(at(49*m), wire.AITurnEnd))

	want := checkAgentReplay(t, full)
	checkBounded(t, full)
	for _, row := range want.Minutes {
		if row.Minute.Equal(at(2*m)) && (row.Label != LabelYou || row.Credited != 60 || len(row.Cuts) != 0) {
			t.Fatalf("minute +2 = %+v, want you with 60 s and no cut", row)
		}
	}
}

// replyMinute is the single minute t0 with the person at the machine
// (idle 1 s) in focus kind focus, no heartbeats and no prompts, and the given
// AI events of one OpenCode session.
func replyMinute(focus string, ai ...wire.AIEvent) Input {
	in := Input{Window: Window{From: t0, To: at(time.Minute)}, AI: ai, DataEnd: at(Lookahead)}
	for x := at(-30 * time.Minute); x.Before(at(5 * time.Minute)); x = x.Add(5 * time.Second) {
		in.Activity = append(in.Activity, wire.ActivitySample{Time: x, IdleSeconds: 1, FocusKind: focus})
	}
	return in
}

func openCodeEvent(d time.Duration, kind wire.AIEventKind, decision string) wire.AIEvent {
	return wire.AIEvent{Time: at(d), Kind: kind, Harness: wire.HarnessOpenCode, SessionID: "s", Project: "app", Decision: decision}
}

// OpenCode reports the person's reply to a permission ask as a
// permission_ask with a Decision. That reply is the person's own signal
// whether or not the engine remembers the ask it answers: the ask may be
// before the caller's data, or its turn may have timed out. With fresh input
// and browser or unknown focus, the reply makes the session active, so the
// minute is the person's.
func TestExplicitReplyIsHumanSignal(t *testing.T) {
	m := time.Minute
	reply := openCodeEvent(0, wire.AIPermissionAsk, "allow")
	tests := []struct {
		name  string
		focus string
		ai    []wire.AIEvent
	}{
		{"no ask seen, browser", "browser", []wire.AIEvent{openCodeEvent(-2*m, wire.AIToolStart, ""), reply}},
		{"no ask seen, unknown focus", "unknown", []wire.AIEvent{openCodeEvent(-2*m, wire.AIToolStart, ""), reply}},
		{"reply alone", "browser", []wire.AIEvent{reply}},
		{"earlier ask timed out", "browser", []wire.AIEvent{openCodeEvent(-20*m, wire.AIPermissionAsk, ""), reply}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := checkBounded(t, replyMinute(tc.focus, tc.ai...))
			if len(res.Minutes) != 1 {
				t.Fatalf("rows %+v, want one", res.Minutes)
			}
			if got := res.Minutes[0]; got.Label != LabelYou || got.Credited != 60 || got.Project != "app" {
				t.Fatalf("minute of an explicit reply = %+v, want you, 60 s, app", got)
			}
		})
	}
}

// A reply that ends a live wait is one signal, not two: with one human
// heartbeat in another project in the same minute, the minute splits evenly.
func TestExplicitReplyCountsOnce(t *testing.T) {
	in := replyMinute("browser",
		openCodeEvent(-time.Minute, wire.AIPermissionAsk, ""),
		openCodeEvent(0, wire.AIPermissionAsk, "allow"))
	in.Heartbeats = []Heartbeat{{Time: at(30 * time.Second), Project: "lib", Entity: "/w/lib/a.go", IsWrite: true}}
	res := checkBounded(t, in)
	got := map[string]int{}
	for _, row := range res.Minutes {
		got[row.Project] = row.Credited
	}
	if got["app"] != 30 || got["lib"] != 30 {
		t.Fatalf("split %v, want app 30 s and lib 30 s", got)
	}
}
