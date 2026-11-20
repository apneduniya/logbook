package struggle

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixturePaths(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no fixtures in testdata")
	}
	return paths
}

// TestGolden runs every scenario in testdata and compares the result with its
// .golden file. Run with -update to rewrite them, then review the diff.
func TestGolden(t *testing.T) {
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			f := loadFixture(t, path)
			got := renderResult(Compute(expandFixture(t, f)))
			golden := strings.TrimSuffix(path, ".json") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./pkg/struggle -update)", err)
			}
			if got != string(want) {
				t.Errorf("%s: result differs from %s (run with -update and review the diff)\n%s",
					f.Description, golden, firstDiff(string(want), got))
			}
		})
	}
}

func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(w), len(g)) {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return "line " + strconv.Itoa(i+1) + ":\n- " + a + "\n+ " + b
		}
	}
	return ""
}

// windowRelative lists the flags computed within the window, which the
// server recomputes over whole days (ADR 0012); final-minute comparisons
// ignore them.
var windowRelative = []string{FlagOTelMismatch, FlagLongDay, FlagScriptedInput}

// finalKey renders a minute without its window-relative flags.
func finalKey(m Minute) string {
	flags := slices.DeleteFunc(slices.Clone(m.Flags), func(f string) bool { return slices.Contains(windowRelative, f) })
	return renderResult(Result{Minutes: []Minute{{m.Minute, m.Project, m.Label, m.Credited, m.Cuts, flags}}})
}

// minutesIn groups a result's rows in [from, to) by minute, as finalKeys.
func minutesIn(res Result, from, to time.Time) map[time.Time][]string {
	out := map[time.Time][]string{}
	for _, m := range res.Minutes {
		if !m.Minute.Before(from) && m.Minute.Before(to) {
			out[m.Minute] = append(out[m.Minute], finalKey(m))
		}
	}
	return out
}

// TestFinalMinutesMatchAgentReplay replays every scenario the way the agent
// runs the engine (ADR 0010, ADR 0012): every minute, over the last 60
// minutes, seeing only the events of the last 90 minutes. The agent's last
// accepted rewrite of a minute happens when it is 25 to 30 minutes old; at
// that age the minute's label, project, credit, cuts and window-independent
// flags must equal a re-run over the whole scenario.
func TestFinalMinutesMatchAgentReplay(t *testing.T) {
	for _, path := range fixturePaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			checkAgentReplay(t, withLateSamples(expandFixture(t, loadFixture(t, path))))
		})
	}
}

// checkAgentReplay replays full as the agent sees it, minute by minute, with
// events in arrival order (sliceInput), and compares every minute the agent
// finalises with a re-run over all of full. It returns the full run.
func checkAgentReplay(t *testing.T, full Input) Result {
	t.Helper()
	server := Compute(full)
	fullFinal := full.Window.To.Add(-Lookahead)
	for now := full.Window.From.Add(Lookahead + time.Minute); !now.After(full.Window.To); now = now.Add(time.Minute) {
		agent := sliceInput(full, now.Add(-ContextBefore-finalAge), now)
		agent.Window = Window{From: now.Add(-time.Hour), To: now}
		from := maxTime(full.Window.From, now.Add(-finalAge))
		to := minTime(now.Add(-Lookahead), fullFinal)
		compareMinutes(t, "agent at "+now.Format("15:04"), Compute(agent), server, from, to)
	}
	return server
}

// cursorToolCall is what Cursor's hooks deliver for a tool call that runs
// from start to end (package cursor): only a tool_end, stamped when it
// arrives. Cursor has no hook when a tool starts, and a tool_start
// back-dated to start would arrive at end, possibly after minutes it would
// change were final (arrivals).
func cursorToolCall(ev func(time.Time, wire.AIEventKind) wire.AIEvent, _, end time.Time) []wire.AIEvent {
	return []wire.AIEvent{ev(end, wire.AIToolEnd)}
}

// withLateSamples adds, for every prompt, activity samples taken just past
// Lookahead and an hour after it that report input at the prompt, as a
// sensor catching up after a stall would. The agent has stopped rewriting
// the prompt's minute before either arrives, so they must not change it.
func withLateSamples(in Input) Input {
	activity := slices.Clone(in.Activity)
	for _, e := range in.AI {
		if e.Kind != wire.AIPromptSubmit {
			continue
		}
		for _, after := range []time.Duration{Lookahead + time.Minute, time.Hour} {
			late := e.Time.Add(after)
			activity = append(activity, wire.ActivitySample{Time: late, IdleSeconds: after.Seconds(), FocusKind: "terminal"})
		}
	}
	in.Activity = activity
	return in
}

// finalAge is ADR 0010's finality age: the agent may rewrite a
// minute until it is this old.
const finalAge = 30 * time.Minute

// TestServerSubWindowMatchesFullRun re-runs every scenario the way the
// server re-runs closed history: windows ending anywhere, with ContextBefore
// of events before and FinalAge after, and DataEnd set. Every minute of the
// sub-window that is final in the full run must match it.
func TestServerSubWindowMatchesFullRun(t *testing.T) {
	for _, path := range fixturePaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			full := withLateSamples(expandFixture(t, loadFixture(t, path)))
			server := Compute(full)
			fullFinal := full.Window.To.Add(-Lookahead)
			for to := full.Window.From.Add(time.Minute); !to.After(full.Window.To); to = to.Add(time.Minute) {
				from := maxTime(full.Window.From, to.Add(-time.Hour))
				sub := sliceInput(full, from.Add(-ContextBefore), to.Add(finalAge))
				sub.Window = Window{From: from, To: to}
				sub.DataEnd = to.Add(finalAge)
				compareMinutes(t, "server window ending "+to.Format("15:04"), Compute(sub), server, from, minTime(to, fullFinal))
			}
		})
	}
}

func compareMinutes(t *testing.T, what string, got, want Result, from, to time.Time) {
	t.Helper()
	g, w := minutesIn(got, from, to), minutesIn(want, from, to)
	for m := from; m.Before(to); m = m.Add(time.Minute) {
		if !slices.Equal(g[m], w[m]) {
			t.Fatalf("%s disagrees on final minute %s:\ngot:    %q\nre-run: %q", what, m.Format("15:04"), g[m], w[m])
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// sliceInput keeps the events in [from, to) that had arrived by to, as the
// agent's local database would return them at time to. Every event arrives
// at its Time except as arrivals says.
func sliceInput(in Input, from, to time.Time) Input {
	keep := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	out := Input{Repos: in.Repos, TZ: in.TZ}
	arrived := arrivals(in.AI)
	for i, e := range in.AI {
		if keep(e.Time) && arrived[i].Before(to) {
			out.AI = append(out.AI, e)
		}
	}
	for _, s := range in.Activity {
		if keep(s.Time) {
			out.Activity = append(out.Activity, s)
		}
	}
	for _, h := range in.Heartbeats {
		if keep(h.Time) {
			out.Heartbeats = append(out.Heartbeats, h)
		}
	}
	for _, a := range in.ActiveTime {
		if keep(a.From) && !a.To.After(to) {
			out.ActiveTime = append(out.ActiveTime, a)
		}
	}
	return out
}

// arrivals returns when the agent receives each of ai's events. Most hooks
// run as the event happens, so an event arrives at its Time. Cursor runs no
// hook when a tool starts (docs/agent/harness.md), so a Cursor tool_start
// stamped earlier can only arrive with its session's next tool_end, and one
// with no later tool_end never arrives. With arrivals, every replay in these
// tests sees events in arrival order: a minute final when an event arrived
// must not change because of it.
func arrivals(ai []wire.AIEvent) []time.Time {
	order := make([]int, len(ai))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return compareAI(ai[a], ai[b]) })
	out := make([]time.Time, len(ai))
	pending := map[string][]int{} // Cursor session -> tool_starts not yet delivered
	never := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, i := range order {
		e := ai[i]
		out[i] = e.Time
		if e.Harness != wire.HarnessCursor {
			continue
		}
		switch e.Kind {
		case wire.AIToolStart:
			out[i] = never
			pending[e.SessionID] = append(pending[e.SessionID], i)
		case wire.AIToolEnd:
			for _, j := range pending[e.SessionID] {
				out[j] = e.Time
			}
			pending[e.SessionID] = nil
		}
	}
	return out
}

// TestShuffledInputGivesSameResult is the determinism property: shuffling
// every input slice, and duplicating events, never changes the result.
func TestShuffledInputGivesSameResult(t *testing.T) {
	for _, path := range fixturePaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			in := expandFixture(t, loadFixture(t, path))
			want := renderResult(Compute(in))
			for seed := range uint64(4) {
				r := rng(seed + 1)
				shuffled := in
				shuffled.Activity = shuffle(&r, in.Activity)
				shuffled.Heartbeats = shuffle(&r, in.Heartbeats)
				shuffled.AI = shuffle(&r, in.AI)
				shuffled.ActiveTime = shuffle(&r, in.ActiveTime)
				shuffled.Repos = shuffle(&r, in.Repos)
				if seed%2 == 1 { // duplicates are dropped
					shuffled.Heartbeats = append(shuffled.Heartbeats, in.Heartbeats...)
					shuffled.AI = append(shuffled.AI, in.AI...)
					shuffled.Activity = append(shuffled.Activity, in.Activity...)
				}
				if got := renderResult(Compute(shuffled)); got != want {
					t.Fatalf("seed %d: shuffled input changed the result\n%s", seed, firstDiff(want, got))
				}
			}
		})
	}
}

func shuffle[T any](r *rng, s []T) []T {
	out := slices.Clone(s)
	for i := len(out) - 1; i > 0; i-- {
		j := int(r.float() * float64(i+1))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// TestResultInvariants checks the output contract on every scenario.
func TestResultInvariants(t *testing.T) {
	for _, path := range fixturePaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			in := expandFixture(t, loadFixture(t, path))
			res := Compute(in)
			if res.RulesVersion != RulesVersion {
				t.Fatalf("rules version %q", res.RulesVersion)
			}
			from := in.Window.From.UTC().Truncate(time.Minute)
			to := in.Window.To.UTC().Truncate(time.Minute)
			perMinute := map[time.Time]int{}
			for k, m := range res.Minutes {
				checkMinute(t, m, from, to)
				if k > 0 {
					prev := res.Minutes[k-1]
					if prev.Minute.After(m.Minute) || (prev.Minute.Equal(m.Minute) && prev.Project >= m.Project) {
						t.Fatalf("rows not sorted by (minute, project) at %d", k)
					}
				}
				perMinute[m.Minute] += m.Credited
				if perMinute[m.Minute] > 60 {
					t.Fatalf("%s: more than 60 s credited", m.Minute)
				}
			}
		})
	}
}

func checkMinute(t *testing.T, m Minute, from, to time.Time) {
	t.Helper()
	switch {
	case m.Minute.Location() != time.UTC || !m.Minute.Equal(m.Minute.Truncate(time.Minute)):
		t.Fatalf("%v: not a whole UTC minute", m.Minute)
	case m.Minute.Before(from) || !m.Minute.Before(to):
		t.Fatalf("%v: outside the window", m.Minute)
	case m.Credited < 0 || m.Credited > 60:
		t.Fatalf("%v: credited %d", m.Minute, m.Credited)
	case m.Label != LabelYou && m.Credited != 0:
		t.Fatalf("%v: %s minute credited", m.Minute, m.Label)
	case m.Label == LabelIdle && m.Project == "":
		t.Fatalf("%v: idle minute without a project emitted", m.Minute)
	case m.Cuts == nil || m.Flags == nil:
		t.Fatalf("%v: nil cuts or flags", m.Minute)
	case !slices.IsSorted(m.Cuts) || !slices.IsSorted(m.Flags):
		t.Fatalf("%v: cuts or flags not sorted", m.Minute)
	case strings.ContainsAny(m.Project, `/\`):
		t.Fatalf("%v: project %q looks like a path", m.Minute, m.Project)
	}
}
