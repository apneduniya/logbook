package crediting

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// realEngineRunner is a Runner that only computes, with the real engine.
func realEngineRunner() *Runner {
	return New(Options{Compute: struggle.Compute, TZ: time.UTC})
}

// fullRun computes the whole span in one engine call with the mapping of
// its last minute, which is what a run gives when the mapping never
// changes.
func fullRun(s span, ev localdb.Events, active []struggle.ActiveTime, repos []struggle.Repo) struggle.Result {
	res := struggle.Compute(engineInput(s, ev, active, repos, time.UTC))
	var out struggle.Result
	out.RulesVersion = res.RulesVersion
	for _, m := range res.Minutes {
		if at := m.Minute.UTC(); !at.Before(s.from) && at.Before(s.to) {
			out.Minutes = append(out.Minutes, m)
		}
	}
	return out
}

func minuteAt(res struggle.Result, at time.Time) []struggle.Minute {
	var out []struggle.Minute
	for _, m := range res.Minutes {
		if m.Minute.Equal(at) {
			out = append(out, m)
		}
	}
	return out
}

// A group of minutes that ends before the run does must still see the data
// as running to the run's end: a turn that went quiet more than 10 minutes
// before the run's end has ended, even if the group itself ends sooner.
func TestEarlierMappingGroupSeesTheRunsDataEnd(t *testing.T) {
	prompt := func(at time.Time) wire.AIEvent {
		return wire.AIEvent{Time: at, Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit,
			SessionID: "cc", Cwd: "/src/app", PromptChars: 40, PromptHash: "same"}
	}
	ev := localdb.Events{
		AI: []wire.AIEvent{
			prompt(at("11:40").Add(5 * time.Second)),
			prompt(at("11:50").Add(5 * time.Second)),
			prompt(at("12:00").Add(5 * time.Second)),
		},
		Activity: []wire.ActivitySample{{Time: at("12:02").Add(30 * time.Second), IdleSeconds: 1, FocusKind: "terminal"}},
	}
	mapping := []localdb.RepoMapping{
		{Since: at("10:00"), Path: "/src/app", Project: "app"},
		// A change that maps no event differently.
		{Since: at("12:34").Add(5 * time.Second), Path: "/src/other", Project: "other"},
	}
	s := spanAt(at("12:35").Add(5 * time.Second))
	if g := repoGroups(mapping, s.from, s.to); len(g) != 2 || !g[0].to.Equal(at("12:05")) {
		t.Fatalf("groups = %+v, want two with the first ending at 12:05", g)
	}

	got := minuteAt(realEngineRunner().computeByMapping(s, ev, nil, mapping), at("12:02"))
	want := minuteAt(fullRun(s, ev, nil, ReposAt(mapping, s.to.Add(-time.Minute))), at("12:02"))
	if len(want) != 1 || want[0].Credited != 60 || len(want[0].Cuts) != 0 {
		t.Fatalf("full run 12:02 = %+v, want 60 s with no cuts", want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grouped 12:02 = %+v, want %+v", got, want)
	}
}

// When no mapping change moves any event to another project, computing by
// mapping group must give exactly what one computation over the whole span
// gives, whatever the events.
func TestGroupedComputationMatchesFullRun(t *testing.T) {
	for seed := range uint64(150) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 7))
			now := at("12:35").Add(5 * time.Second)
			s := spanAt(now)
			ev, active := randomEvents(rng, s.history, s.to)
			// Changes only to paths no event uses, at random times in
			// the rewrite window, so groups split but projects don't move.
			mapping := []localdb.RepoMapping{
				{Since: at("09:00"), Path: "/src/app", Project: "app"},
				{Since: at("09:00"), Path: "/src/lib", Project: "lib"},
			}
			for i := range 1 + rng.IntN(3) {
				// Recorded in the last half hour: it splits the open minutes off.
				since := now.Add(-time.Duration(rng.IntN(int(FinalAfter/time.Second))) * time.Second)
				mapping = append(mapping, localdb.RepoMapping{Since: since, Path: fmt.Sprintf("/unused/%d", i), Project: "unused"})
			}
			r := realEngineRunner()
			r.opt.TZ = time.UTC
			got := r.computeByMapping(s, ev, active, mapping)
			want := fullRun(s, ev, active, ReposAt(mapping, s.from))
			if !reflect.DeepEqual(normalized(got), normalized(want)) {
				t.Fatalf("grouped result differs from one full computation\n got %+v\nwant %+v", got.Minutes, want.Minutes)
			}
		})
	}
}

// normalized returns the minutes, with nil and empty alike.
func normalized(r struggle.Result) []struggle.Minute {
	if r.Minutes == nil {
		return []struggle.Minute{}
	}
	return r.Minutes
}

// randomEvents makes a random mix of prompts, tool calls, heartbeats,
// activity and active time in [from, to), on the mapped paths only.
func randomEvents(rng *rand.Rand, from, to time.Time) (localdb.Events, []struggle.ActiveTime) {
	var ev localdb.Events
	var active []struggle.ActiveTime
	// All data stops at a random point, so turns go quiet at different
	// distances from the end of the run.
	span := 1 + rng.IntN(int(to.Sub(from)/time.Second))
	when := func() time.Time { return from.Add(time.Duration(rng.IntN(span)) * time.Second) }
	cwds := []string{"/src/app", "/src/lib"}
	projects := []string{"app", "lib"}
	kinds := []wire.AIEventKind{wire.AIPromptSubmit, wire.AIToolStart, wire.AIToolEnd, wire.AITurnEnd, wire.AIPermissionAsk}
	for range 10 + rng.IntN(40) {
		k := kinds[rng.IntN(len(kinds))]
		e := wire.AIEvent{Time: when(), Harness: wire.HarnessClaudeCode, Kind: k,
			SessionID: fmt.Sprint("s", rng.IntN(2)), Cwd: cwds[rng.IntN(2)]}
		if k == wire.AIPromptSubmit {
			e.PromptChars = 1 + rng.IntN(200)
			e.PromptHash = fmt.Sprint("h", rng.IntN(2))
		}
		ev.AI = append(ev.AI, e)
	}
	for range rng.IntN(60) {
		ev.Heartbeats = append(ev.Heartbeats, wire.Heartbeat{
			Time: float64(when().Unix()), Entity: "/src/app/main.go", Project: projects[rng.IntN(2)],
			IsWrite: rng.IntN(3) == 0, Lineno: new(1 + rng.IntN(50)),
		})
	}
	focus := []string{"terminal", "editor", "browser", "ai", "other"}
	quiet := from.Add(time.Duration(span) * time.Second) // all data stops here
	for t := from; t.Before(quiet); t = t.Add(10 * time.Second) {
		if rng.IntN(4) == 0 {
			continue
		}
		ev.Activity = append(ev.Activity, wire.ActivitySample{Time: t, IdleSeconds: float64(rng.IntN(400)),
			FocusKind: focus[rng.IntN(len(focus))]})
	}
	for range rng.IntN(5) {
		f := when()
		active = append(active, struggle.ActiveTime{From: f, To: f.Add(time.Minute), SessionID: fmt.Sprint("s", rng.IntN(2)),
			UserSecs: float64(rng.IntN(20)), CLISecs: float64(rng.IntN(20))})
	}
	return ev, active
}
