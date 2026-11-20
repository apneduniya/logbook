package struggle

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func TestRepoIndexProject(t *testing.T) {
	idx := newRepoIndex([]Repo{
		{Path: "/home/sam/app", Project: "app"},
		{Path: "/home/sam/app/vendor/lib/", Project: "lib"},
		{Path: `C:\Users\Sam\code\web`, Project: "web"},
		{Path: "/home/sam/app", Project: "aaa"}, // same path: the smaller name wins
		{Path: "", Project: "ignored"},
		{Path: "/x", Project: ""},
	})
	tests := []struct {
		path, want string
	}{
		{"/home/sam/app", "aaa"},
		{"/home/sam/app/main.go", "aaa"},
		{"/home/sam/app/vendor/lib/x.go", "lib"},
		{"/home/sam/apple/main.go", ""},
		{"/home/sam", ""},
		{`c:\users\sam\code\web\src\index.ts`, "web"},
		{`C:/Users/Sam/code/web`, "web"},
		{`C:/Users/Sam/code/website`, ""},
		{"", ""},
		{"/x/y", ""},
	}
	for _, tt := range tests {
		if got := idx.project(tt.path); got != tt.want {
			t.Errorf("project(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestSplitSeconds(t *testing.T) {
	tests := []struct {
		name   string
		counts []projectCount
		want   []projectCount
	}{
		{"one project", []projectCount{{"a", 4}}, []projectCount{{"a", 60}}},
		{"3:2", []projectCount{{"a", 3}, {"b", 2}}, []projectCount{{"a", 36}, {"b", 24}}},
		{"even three-way", []projectCount{{"a", 1}, {"b", 1}, {"c", 1}}, []projectCount{{"a", 20}, {"b", 20}, {"c", 20}}},
		{"remainder by size", []projectCount{{"a", 1}, {"b", 2}, {"c", 4}}, []projectCount{{"a", 9}, {"b", 17}, {"c", 34}}},
		{"remainder tie by name", []projectCount{{"a", 1}, {"b", 1}, {"c", 1}, {"d", 1}, {"e", 1}, {"f", 1}, {"g", 1}},
			[]projectCount{{"a", 9}, {"b", 9}, {"c", 9}, {"d", 9}, {"e", 8}, {"f", 8}, {"g", 8}}},
		{"tiny share dropped", []projectCount{{"a", 200}, {"b", 1}}, []projectCount{{"a", 60}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSeconds(60, tt.counts)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			sum := 0
			for _, c := range got {
				sum += c.count
			}
			if sum != 60 {
				t.Fatalf("sum %d", sum)
			}
		})
	}
}

func TestTouched(t *testing.T) {
	tl := &timeline{base: t0, n: 10}
	tests := []struct {
		name   string
		a, b   time.Duration
		lo, hi int
	}{
		{"inside one minute", 10 * time.Second, 20 * time.Second, 0, 1},
		{"zero length", 90 * time.Second, 90 * time.Second, 1, 2},
		{"ends on a boundary", 30 * time.Second, 2 * time.Minute, 0, 2},
		{"crosses", 50 * time.Second, 130 * time.Second, 0, 3},
		{"clipped", -5 * time.Minute, 30 * time.Minute, 0, 10},
		{"reversed", 5 * time.Minute, time.Minute, 0, 0},
		{"before the timeline", -5 * time.Minute, -3 * time.Minute, 0, 0},
	}
	for _, tt := range tests {
		if lo, hi := tl.touched(at(tt.a), at(tt.b)); lo != tt.lo || hi != tt.hi {
			t.Errorf("%s: touched = [%d,%d), want [%d,%d)", tt.name, lo, hi, tt.lo, tt.hi)
		}
	}
}

func TestFloorMinutes(t *testing.T) {
	for d, want := range map[time.Duration]int{
		0: 0, 59 * time.Second: 0, time.Minute: 1, -time.Second: -1, -time.Minute: -1, -61 * time.Second: -2,
	} {
		if got := floorMinutes(d); got != want {
			t.Errorf("floorMinutes(%v) = %d, want %d", d, got, want)
		}
	}
}

func ai(d time.Duration, kind wire.AIEventKind, session string) aiEvent {
	return aiEvent{t: at(d), kind: kind, session: session, project: "app"}
}

func TestBuildTurns(t *testing.T) {
	type want struct {
		start, end time.Duration
		prompt     bool
		byTurnEnd  bool
		busy       int // busy spans
	}
	tests := []struct {
		name    string
		events  []aiEvent
		horizon time.Duration
		want    []want
	}{
		{
			name: "prompt to turn_end",
			events: []aiEvent{ai(0, wire.AIPromptSubmit, "s"), ai(time.Minute, wire.AIToolStart, "s"),
				ai(2*time.Minute, wire.AIToolEnd, "s"), ai(3*time.Minute, wire.AITurnEnd, "s")},
			horizon: time.Hour,
			want:    []want{{0, 3 * time.Minute, true, true, 1}},
		},
		{
			name: "permission wait splits busy time",
			events: []aiEvent{ai(0, wire.AIPromptSubmit, "s"), ai(time.Minute, wire.AIPermissionAsk, "s"),
				ai(5*time.Minute, wire.AIToolEnd, "s"), ai(6*time.Minute, wire.AITurnEnd, "s")},
			horizon: time.Hour,
			want:    []want{{0, 6 * time.Minute, true, true, 2}},
		},
		{
			name: "next prompt ends the turn (interrupted)",
			events: []aiEvent{ai(0, wire.AIPromptSubmit, "s"), ai(time.Minute, wire.AIToolStart, "s"),
				ai(2*time.Minute, wire.AIPromptSubmit, "s"), ai(3*time.Minute, wire.AITurnEnd, "s")},
			horizon: time.Hour,
			want:    []want{{0, 2 * time.Minute, true, false, 1}, {2 * time.Minute, 3 * time.Minute, true, true, 1}},
		},
		{
			name: "no events for more than 10 minutes ends the turn at its last event",
			events: []aiEvent{ai(0, wire.AIPromptSubmit, "s"), ai(time.Minute, wire.AIToolStart, "s"),
				ai(12*time.Minute, wire.AIToolStart, "s"), ai(13*time.Minute, wire.AITurnEnd, "s")},
			horizon: time.Hour,
			want:    []want{{0, time.Minute, true, false, 1}, {12 * time.Minute, 13 * time.Minute, false, true, 1}},
		},
		{
			name:    "open turn near the horizon runs to the horizon",
			events:  []aiEvent{ai(0, wire.AIPromptSubmit, "s"), ai(time.Minute, wire.AIToolStart, "s")},
			horizon: 5 * time.Minute,
			want:    []want{{0, 5 * time.Minute, true, false, 1}},
		},
		{
			name:    "open turn long before the horizon timed out",
			events:  []aiEvent{ai(0, wire.AIPromptSubmit, "s"), ai(time.Minute, wire.AIToolStart, "s")},
			horizon: time.Hour,
			want:    []want{{0, time.Minute, true, false, 1}},
		},
		{
			name:    "permission ask and turn_end outside a turn are ignored",
			events:  []aiEvent{ai(0, wire.AIPermissionAsk, "s"), ai(time.Minute, wire.AITurnEnd, "s"), ai(2*time.Minute, wire.AIToolEnd, "s")},
			horizon: time.Hour,
			want:    nil,
		},
		{
			name: "sessions are independent",
			events: []aiEvent{ai(0, wire.AIPromptSubmit, "b"), ai(30*time.Second, wire.AIPromptSubmit, "a"),
				ai(time.Minute, wire.AITurnEnd, "b"), ai(2*time.Minute, wire.AITurnEnd, "a")},
			horizon: time.Hour,
			want:    []want{{0, time.Minute, true, true, 1}, {30 * time.Second, 2 * time.Minute, true, true, 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildTurns(tt.events, at(tt.horizon))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d turns, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				g := got[i]
				if !g.start.Equal(at(w.start)) || !g.end.Equal(at(w.end)) || g.prompt != w.prompt ||
					g.endedByTurnEnd != w.byTurnEnd || len(g.busySpans()) != w.busy || g.project != "app" {
					t.Errorf("turn %d = %+v (busy %v), want %+v", i, g, g.busySpans(), w)
				}
			}
		})
	}
}

func promptTurn(start, end time.Duration, chars int, hash string) turn {
	return turn{session: "s", start: at(start), end: at(end), prompt: true, chars: chars, hash: hash, endedByTurnEnd: true}
}

func TestLoopSpans(t *testing.T) {
	m := time.Minute
	tests := []struct {
		name  string
		turns []turn
		cut   []time.Duration // start of each cut turn
	}{
		{"two sends are fine", []turn{promptTurn(0, m, 10, "h"), promptTurn(2*m, 3*m, 10, "h")}, nil},
		{"third send is cut", []turn{promptTurn(0, m, 10, "h"), promptTurn(2*m, 3*m, 10, "h"), promptTurn(4*m, 5*m, 10, "h")}, []time.Duration{4 * m}},
		{"outside 30 minutes", []turn{promptTurn(0, m, 10, "h"), promptTurn(20*m, 21*m, 10, "h"), promptTurn(31*m, 32*m, 10, "h")}, nil},
		{"exactly 30 minutes counts", []turn{promptTurn(0, m, 10, "h"), promptTurn(20*m, 21*m, 10, "h"), promptTurn(30*m, 31*m, 10, "h")}, []time.Duration{30 * m}},
		{"no hash never loops", []turn{promptTurn(0, m, 10, ""), promptTurn(2*m, 3*m, 10, ""), promptTurn(4*m, 5*m, 10, "")}, nil},
		{"other sessions don't count", []turn{promptTurn(0, m, 10, "h"), {session: "t", start: at(2 * m), end: at(3 * m), prompt: true, hash: "h"}, promptTurn(4*m, 5*m, 10, "h")}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []time.Duration
			for _, s := range loopSpans(tt.turns) {
				got = append(got, s.from.Sub(t0))
			}
			if !slices.Equal(got, tt.cut) {
				t.Fatalf("cut %v, want %v", got, tt.cut)
			}
		})
	}
}

func TestRubberStampSpans(t *testing.T) {
	m, s := time.Minute, time.Second
	stamps := func(n int, gap time.Duration, chars int, turnLen time.Duration) []turn {
		ts := []turn{promptTurn(0, m, 200, "first")}
		for i := range n {
			start := ts[len(ts)-1].end.Sub(t0) + gap
			ts = append(ts, promptTurn(start, start+turnLen, chars, "s"+strings.Repeat("x", i)))
		}
		return ts
	}
	tests := []struct {
		name  string
		turns []turn
		hbs   []heartbeat
		want  int // cut spans
	}{
		{"two stamps are not a streak", stamps(2, 5*s, 3, m), nil, 0},
		{"three stamps cut the 2nd and 3rd", stamps(3, 5*s, 3, m), nil, 2},
		{"five stamps cut all but the first", stamps(5, 5*s, 3, m), nil, 4},
		{"slow replies are not stamps", stamps(4, 20*s, 3, m), nil, 0},
		{"long prompts are not stamps", stamps(4, 5*s, 26, m), nil, 0},
		{"unknown length is not a stamp", stamps(4, 5*s, 0, m), nil, 0},
		{"editing in between breaks it", stamps(3, 10*s, 3, m), []heartbeat{{t: at(m + 3*s)}}, 0},
		{"AI edits in between don't", stamps(3, 10*s, 3, m), []heartbeat{{t: at(m + 3*s), ai: true}}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rubberStampSpans(tt.turns, tt.hbs); len(got) != tt.want {
				t.Fatalf("got %d spans %v, want %d", len(got), got, tt.want)
			}
		})
	}

	// The second stamp's turn is cut retroactively only within the finality
	// horizon before the third stamp.
	long := stamps(3, 5*s, 3, 45*m)
	spans := rubberStampSpans(long, nil)
	third := long[3].start
	if want := third.Add(-finalityHorizon); !spans[0].from.Equal(want) {
		t.Fatalf("retroactive cut starts at %v, want %v", spans[0].from, want)
	}
}

func TestRegularSpans(t *testing.T) {
	regular := func(n int, every time.Duration, jitter func(int) time.Duration) []time.Time {
		var ts []time.Time
		for i := range n {
			ts = append(ts, at(time.Duration(i)*every+jitter(i)))
		}
		return ts
	}
	none := func(int) time.Duration { return 0 }
	human := func(i int) time.Duration { return time.Duration((i*7919)%2000) * time.Millisecond }
	tests := []struct {
		name string
		ts   []time.Time
		want bool
	}{
		{"30 intervals exactly every 30 s", regular(31, 30*time.Second, none), true},
		{"29 intervals is too short", regular(30, 30*time.Second, none), false},
		{"human jitter", regular(60, 30*time.Second, human), false},
		{"small jitter (0.1 s) is still scripted", regular(40, 30*time.Second, func(i int) time.Duration { return time.Duration(i%2) * 100 * time.Millisecond }), true},
		{"throttle cadence (120 s) is ignored", regular(40, 120*time.Second, none), false},
		{"bulk duplicates are ignored", regular(40, 0, none), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(regularSpans(tt.ts, scriptedHBShortest, scriptedHBLongest)) > 0; got != tt.want {
				t.Fatalf("regular = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQuietInstants(t *testing.T) {
	mk := func(sec float64, idle float64) sample {
		ts := at(time.Duration(sec * float64(time.Second)))
		d := time.Duration(idle * float64(time.Second))
		return sample{t: ts, idle: d, input: ts.Add(-d), hasInput: true}
	}
	samples := []sample{
		mk(5, 0.2), mk(10, 0.3), // continuous typing: never quiet
		mk(15, 4), mk(20, 9), // one input at 11 s, seen twice, quiet
		{t: at(25 * time.Second)}, // unusable reading
		mk(30, 1), mk(35, 6),      // input at 29 s, quiet
	}
	got := quietInstants(samples)
	want := []time.Time{at(11 * time.Second), at(29 * time.Second)}
	if !slices.EqualFunc(got, want, time.Time.Equal) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestComputeEdgeCases(t *testing.T) {
	t.Run("empty or reversed window", func(t *testing.T) {
		for _, w := range []Window{{}, {From: at(time.Hour), To: t0}, {From: at(10 * time.Second), To: at(50 * time.Second)}} {
			res := Compute(Input{Window: w})
			if res.RulesVersion != RulesVersion || res.Minutes == nil || len(res.Minutes) != 0 {
				t.Fatalf("%v: got %+v", w, res)
			}
		}
	})
	t.Run("window is truncated to whole minutes and capped", func(t *testing.T) {
		p := prepare(Input{Window: Window{From: at(30 * time.Second), To: at(MaxWindow + time.Hour)}})
		if !p.from.Equal(t0) || !p.to.Equal(t0.Add(MaxWindow)) || p.tz != time.UTC {
			t.Fatalf("from %v to %v tz %v", p.from, p.to, p.tz)
		}
	})
	t.Run("unusable idle readings are not input", func(t *testing.T) {
		in := Input{Window: Window{From: t0, To: at(5 * time.Minute)}}
		for i := range 60 {
			in.Activity = append(in.Activity, wire.ActivitySample{
				Time: at(time.Duration(i) * 5 * time.Second), IdleSeconds: []float64{math.NaN(), -1, math.Inf(1)}[i%3], FocusKind: "editor",
			})
		}
		in.Heartbeats = []Heartbeat{{Time: at(time.Minute), Project: "app", IsWrite: true}}
		for _, m := range Compute(in).Minutes {
			if m.Label == LabelYou {
				t.Fatalf("%v credited from unusable idle readings", m.Minute)
			}
		}
	})
	t.Run("AI-attributed heartbeats are not presence on low-signal devices", func(t *testing.T) {
		in := Input{Window: Window{From: t0, To: at(10 * time.Minute)}}
		for i := range 20 {
			in.Heartbeats = append(in.Heartbeats, Heartbeat{Time: at(time.Duration(i) * 30 * time.Second), Project: "app", IsWrite: true, AI: true})
		}
		for _, m := range Compute(in).Minutes {
			if m.Label == LabelYou {
				t.Fatalf("%v credited from AI edits", m.Minute)
			}
		}
	})
	t.Run("paths never reach the output", func(t *testing.T) {
		in := Input{Window: Window{From: t0, To: at(5 * time.Minute)}}
		for i := range 60 {
			in.Activity = append(in.Activity, wire.ActivitySample{Time: at(time.Duration(i) * 5 * time.Second), IdleSeconds: 1, FocusKind: "editor"})
		}
		in.Heartbeats = []Heartbeat{{Time: at(time.Minute), Entity: "/home/sam/secret/main.go", IsWrite: true}}
		in.AI = []wire.AIEvent{{Time: at(2 * time.Minute), Kind: wire.AIPromptSubmit, Harness: wire.HarnessCodex, SessionID: "x", Cwd: "/home/sam/secret"}}
		for _, m := range Compute(in).Minutes {
			if strings.Contains(m.Project, "secret") || m.Project != "" {
				t.Fatalf("project %q leaked from a path", m.Project)
			}
		}
	})
	t.Run("prompts on low-signal stretches are not judged automated", func(t *testing.T) {
		in := Input{Window: Window{From: t0, To: at(10 * time.Minute)}}
		in.AI = []wire.AIEvent{
			{Time: at(time.Minute), Kind: wire.AIPromptSubmit, SessionID: "s", Harness: wire.HarnessClaudeCode, Project: "app"},
			{Time: at(5 * time.Minute), Kind: wire.AITurnEnd, SessionID: "s", Harness: wire.HarnessClaudeCode, Project: "app"},
		}
		for _, m := range Compute(in).Minutes {
			if slices.Contains(m.Flags, FlagAutomatedSession) {
				t.Fatalf("%v flagged automated without activity data", m.Minute)
			}
		}
	})
}

func TestOverlapFraction(t *testing.T) {
	m := time.Minute
	tests := []struct {
		a, b time.Duration
		want float64
	}{
		{0, 10 * m, 1},
		{-5 * m, 5 * m, 0.5},
		{50 * m, 70 * m, 0.5},
		{-10 * m, -5 * m, 0},
		{5 * m, 5 * m, 1},
		{60 * m, 60 * m, 0},
	}
	for _, tt := range tests {
		if got := overlapFraction(at(tt.a), at(tt.b), t0, at(60*m)); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("overlap(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
