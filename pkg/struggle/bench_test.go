package struggle

import (
	"fmt"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// benchInput is a 2-hour window (twice what the agent recomputes every
// minute) plus ContextBefore of history, with activity every 5 s, heartbeats
// every ~30 s, an agent turn every 5 minutes with a tool call every ~10 s,
// and OTel points.
func benchInput() Input {
	r := rng(42)
	from := t0
	start := from.Add(-ContextBefore)
	end := from.Add(2 * time.Hour)
	in := Input{
		Window: Window{From: from, To: end},
		Repos:  []Repo{{Path: "/home/sam/app", Project: "app"}, {Path: "/home/sam/lib", Project: "lib"}},
		TZ:     time.FixedZone("IST", 330*60),
	}
	last := start
	for x := start; x.Before(end); x = x.Add(5 * time.Second) {
		if r.float() < 0.8 {
			last = x.Add(-secs(r.between(0, 4)))
		}
		kind := "editor"
		if x.Minute()%7 == 0 {
			kind = "terminal"
		}
		in.Activity = append(in.Activity, wire.ActivitySample{Time: x, IdleSeconds: x.Sub(last).Seconds(), FocusKind: kind, FocusApp: "code"})
	}
	line := 1
	for x := start; x.Before(end); x = x.Add(secs(r.between(20, 40))) {
		line += 3
		l := line
		project, entity := "app", "/home/sam/app/main.go"
		if r.float() < 0.3 {
			project, entity = "lib", "/home/sam/lib/util.go"
		}
		in.Heartbeats = append(in.Heartbeats, Heartbeat{Time: x, Project: project, Entity: entity, IsWrite: r.float() < 0.3, Lineno: &l})
	}
	for k, x := 0, start; x.Before(end); k, x = k+1, x.Add(5*time.Minute) {
		ev := func(t time.Time, kind wire.AIEventKind) wire.AIEvent {
			return wire.AIEvent{Time: t, Harness: wire.HarnessClaudeCode, Kind: kind, SessionID: "bench", Cwd: "/home/sam/app/src", Tool: "Edit"}
		}
		p := ev(x, wire.AIPromptSubmit)
		p.PromptChars, p.PromptHash = 40+k, fmt.Sprintf("%064x", k%4)
		in.AI = append(in.AI, p)
		for t := x.Add(5 * time.Second); t.Before(x.Add(4 * time.Minute)); t = t.Add(secs(r.between(5, 15))) {
			in.AI = append(in.AI, ev(t, wire.AIToolStart), ev(t.Add(2*time.Second), wire.AIToolEnd))
		}
		in.AI = append(in.AI, ev(x.Add(4*time.Minute), wire.AITurnEnd))
		in.ActiveTime = append(in.ActiveTime, ActiveTime{From: x, To: x.Add(5 * time.Minute), SessionID: "bench",
			UserSecs: 120, CLISecs: 180, Harness: wire.HarnessClaudeCode})
	}
	return in
}

// BenchmarkCompute2h must stay well under 50 ms per op on modest hardware:
// the agent runs it every minute.
func BenchmarkCompute2h(b *testing.B) {
	in := benchInput()
	b.ReportMetric(float64(len(in.Activity)+len(in.Heartbeats)+len(in.AI)), "events")
	b.ReportAllocs()
	for b.Loop() {
		Compute(in)
	}
}

func TestBenchInputIsRealistic(t *testing.T) {
	res := Compute(benchInput())
	credited := 0
	for _, m := range res.Minutes {
		credited += m.Credited
	}
	if credited < 100*60 {
		t.Fatalf("benchmark input credits only %d s; it should look like a working session", credited)
	}
}
