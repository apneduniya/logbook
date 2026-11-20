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

// TestFinalMinutesMatchAgentReplay replays every scenario the way the agent
// runs the engine (ADR 0010, ADR 0012): every minute, over the last 60
// minutes, seeing only the events so far plus 30 minutes of history. Once a
// minute is 30 minutes old (final), its label, project, credit, cuts and
// window-independent flags must equal a re-run over the whole scenario.
func TestFinalMinutesMatchAgentReplay(t *testing.T) {
	windowRelative := []string{FlagOTelMismatch, FlagLongDay}
	key := func(m Minute) string {
		flags := slices.DeleteFunc(slices.Clone(m.Flags), func(f string) bool { return slices.Contains(windowRelative, f) })
		return renderResult(Result{Minutes: []Minute{{m.Minute, m.Project, m.Label, m.Credited, m.Cuts, flags}}})
	}
	byMinute := func(res Result, from, to time.Time) map[time.Time][]string {
		out := map[time.Time][]string{}
		for _, m := range res.Minutes {
			if !m.Minute.Before(from) && m.Minute.Before(to) {
				out[m.Minute] = append(out[m.Minute], key(m))
			}
		}
		return out
	}
	for _, path := range fixturePaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			full := expandFixture(t, loadFixture(t, path))
			server := Compute(full)
			for now := full.Window.From.Add(31 * time.Minute); !now.After(full.Window.To); now = now.Add(time.Minute) {
				agent := sliceInput(full, now.Add(-90*time.Minute), now)
				agent.Window = Window{From: now.Add(-time.Hour), To: now}
				from := maxTime(full.Window.From, agent.Window.From)
				final := now.Add(-finalityHorizon)
				want := byMinute(server, from, final)
				got := byMinute(Compute(agent), from, final)
				for m := from; m.Before(final); m = m.Add(time.Minute) {
					if !slices.Equal(got[m], want[m]) {
						t.Fatalf("agent at %s disagrees on final minute %s:\nagent:  %q\nre-run: %q",
							now.Format("15:04"), m.Format("15:04"), got[m], want[m])
					}
				}
			}
		})
	}
}

// sliceInput keeps the events in [from, to), as the agent's local database
// would return them at time to.
func sliceInput(in Input, from, to time.Time) Input {
	keep := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	out := Input{Repos: in.Repos, TZ: in.TZ}
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
	for _, e := range in.AI {
		if keep(e.Time) {
			out.AI = append(out.AI, e)
		}
	}
	for _, a := range in.ActiveTime {
		if keep(a.From) && !a.To.After(to) {
			out.ActiveTime = append(out.ActiveTime, a)
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
