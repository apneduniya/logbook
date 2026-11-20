package struggle

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// fixture is a scenario in testdata/*.json. Instead of thousands of raw
// samples it describes what happened (when the person gave input, which app
// had focus, which heartbeats and agent turns occurred) and expandFixture
// turns that into an Input deterministically. Raw events can be given too.
//
// Times are "HH:MM:SS" (on Date, UTC) or RFC 3339. Durations are seconds.
type fixture struct {
	Description     string `json:"description"`
	Date            string `json:"date"`
	Window          struct{ From, To string }
	TZOffsetMinutes int    `json:"tz_offset_minutes"`
	Seed            uint64 `json:"seed"`
	Repos           []struct {
		Path    string `json:"path"`
		Project string `json:"project"`
	} `json:"repos"`

	// Inputs are stretches where the person gives input, with gaps between
	// inputs drawn from [GapMin, GapMax] seconds.
	Inputs []struct {
		From   string  `json:"from"`
		To     string  `json:"to"`
		GapMin float64 `json:"gap_min"`
		GapMax float64 `json:"gap_max"`
	} `json:"inputs"`
	InputAt []string `json:"input_at"`
	// Samples are stretches where the sensor runs, every Every seconds
	// (default 5), with the given focus kind. Idle time comes from Inputs.
	Samples []struct {
		From  string  `json:"from"`
		To    string  `json:"to"`
		Every float64 `json:"every"`
		Focus string  `json:"focus"`
	} `json:"samples"`
	Heartbeats []struct {
		From     string  `json:"from"`
		To       string  `json:"to"`
		EveryMin float64 `json:"every_min"`
		EveryMax float64 `json:"every_max"`
		Project  string  `json:"project"`
		Entity   string  `json:"entity"`
		// WriteEvery makes every Nth heartbeat a write (0: none).
		WriteEvery int `json:"write_every"`
		// Cursor moves the line number on every heartbeat; Still keeps it fixed.
		Cursor bool `json:"cursor"`
		Still  bool `json:"still"`
		AI     bool `json:"ai"`
	} `json:"heartbeats"`
	// Turns are agent turns: a prompt at Start (or a tool_start with
	// NoPrompt), tool calls every ToolEvery seconds, permission waits, and a
	// turn_end at End unless Open.
	Turns []struct {
		Session    string  `json:"session"`
		Harness    string  `json:"harness"`
		Cwd        string  `json:"cwd"`
		Start      string  `json:"start"`
		End        string  `json:"end"`
		Open       bool    `json:"open"`
		NoPrompt   bool    `json:"no_prompt"`
		Chars      int     `json:"chars"`
		Hash       string  `json:"hash"`
		ToolEvery  float64 `json:"tool_every"`
		Permission []struct {
			At    string `json:"at"`
			Until string `json:"until"`
		} `json:"permission"`
	} `json:"turns"`
	AI []struct {
		At      string `json:"at"`
		Kind    string `json:"kind"`
		Session string `json:"session"`
		Cwd     string `json:"cwd"`
		Chars   int    `json:"chars"`
		Hash    string `json:"hash"`
	} `json:"ai"`
	ActiveTime []struct {
		From     string  `json:"from"`
		To       string  `json:"to"`
		Session  string  `json:"session"`
		UserSecs float64 `json:"user_secs"`
		CLISecs  float64 `json:"cli_secs"`
	} `json:"active_time"`
}

func loadFixture(t testing.TB, path string) fixture {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return f
}

// rng is splitmix64: tiny, and stable across Go releases.
type rng uint64

func (r *rng) float() float64 {
	*r += 0x9e3779b97f4a7c15
	z := uint64(*r)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return float64(z>>11) / (1 << 53)
}

func (r *rng) between(lo, hi float64) float64 { return lo + r.float()*(hi-lo) }

func secs(s float64) time.Duration { return time.Duration(math.Round(s*1000)) * time.Millisecond }

func expandFixture(t testing.TB, f fixture) Input {
	t.Helper()
	at := func(s string) time.Time {
		t.Helper()
		if s == "" {
			t.Fatal("missing time")
		}
		if len(s) == len("15:04:05") {
			s = f.Date + "T" + s + "Z"
		}
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("time %q: %v", s, err)
		}
		return v
	}
	in := Input{
		Window: Window{From: at(f.Window.From), To: at(f.Window.To)},
		TZ:     time.FixedZone("fixture", f.TZOffsetMinutes*60),
	}
	for _, r := range f.Repos {
		in.Repos = append(in.Repos, Repo{Path: r.Path, Project: r.Project})
	}

	var inputs []time.Time
	for k, s := range f.Inputs {
		r := rng(f.Seed*1000 + uint64(k))
		for x := at(s.From); x.Before(at(s.To)); x = x.Add(secs(r.between(s.GapMin, s.GapMax))) {
			inputs = append(inputs, x)
		}
	}
	for _, s := range f.InputAt {
		inputs = append(inputs, at(s))
	}
	slices.SortFunc(inputs, time.Time.Compare)
	for _, s := range f.Samples {
		every := s.Every
		if every == 0 {
			every = 5
		}
		for x := at(s.From); x.Before(at(s.To)); x = x.Add(secs(every)) {
			idle := 3600.0
			if i, _ := slices.BinarySearchFunc(inputs, x, time.Time.Compare); i < len(inputs) && inputs[i].Equal(x) {
				idle = 0
			} else if i > 0 {
				idle = x.Sub(inputs[i-1]).Seconds()
			}
			in.Activity = append(in.Activity, wire.ActivitySample{Time: x, IdleSeconds: idle, FocusKind: s.Focus})
		}
	}

	for k, h := range f.Heartbeats {
		r := rng(f.Seed*1000 + 500 + uint64(k))
		line := 10
		for n, x := 0, at(h.From); x.Before(at(h.To)); n++ {
			hb := Heartbeat{Time: x, Project: h.Project, Entity: h.Entity, AI: h.AI}
			hb.IsWrite = h.WriteEvery > 0 && n%h.WriteEvery == h.WriteEvery-1
			if h.Cursor {
				line += 1 + n%3
			}
			if h.Cursor || h.Still {
				l := line
				hb.Lineno = &l
			}
			in.Heartbeats = append(in.Heartbeats, hb)
			x = x.Add(secs(r.between(h.EveryMin, h.EveryMax)))
		}
	}

	harness := func(h string) wire.Harness {
		if h == "" {
			return wire.HarnessClaudeCode
		}
		return wire.Harness(h)
	}
	for _, tr := range f.Turns {
		ev := func(x time.Time, kind wire.AIEventKind) wire.AIEvent {
			return wire.AIEvent{Time: x, Harness: harness(tr.Harness), Kind: kind, SessionID: tr.Session, Cwd: tr.Cwd}
		}
		start, end := at(tr.Start), at(tr.End)
		if tr.NoPrompt {
			in.AI = append(in.AI, ev(start, wire.AIToolStart))
		} else {
			e := ev(start, wire.AIPromptSubmit)
			e.PromptChars, e.PromptHash = tr.Chars, tr.Hash
			in.AI = append(in.AI, e)
		}
		waiting := func(x time.Time) bool {
			for _, p := range tr.Permission {
				if !x.Before(at(p.At)) && x.Before(at(p.Until)) {
					return true
				}
			}
			return false
		}
		if tr.ToolEvery > 0 {
			for x := start.Add(secs(tr.ToolEvery)); x.Before(end); x = x.Add(secs(tr.ToolEvery)) {
				if !waiting(x) {
					in.AI = append(in.AI, ev(x, wire.AIToolStart), ev(x.Add(time.Second), wire.AIToolEnd))
				}
			}
		}
		for _, p := range tr.Permission {
			in.AI = append(in.AI, ev(at(p.At), wire.AIPermissionAsk), ev(at(p.Until), wire.AIToolEnd))
		}
		if !tr.Open {
			in.AI = append(in.AI, ev(end, wire.AITurnEnd))
		}
	}
	for _, a := range f.AI {
		in.AI = append(in.AI, wire.AIEvent{
			Time: at(a.At), Harness: wire.HarnessClaudeCode, Kind: wire.AIEventKind(a.Kind),
			SessionID: a.Session, Cwd: a.Cwd, PromptChars: a.Chars, PromptHash: a.Hash,
		})
	}
	for _, a := range f.ActiveTime {
		in.ActiveTime = append(in.ActiveTime, ActiveTime{
			From: at(a.From), To: at(a.To), SessionID: a.Session,
			UserSecs: a.UserSecs, CLISecs: a.CLISecs, Harness: wire.HarnessClaudeCode,
		})
	}
	return in
}

// renderResult prints a Result as a readable table with a summary, which is
// what the .golden files hold.
func renderResult(r Result) string {
	var b strings.Builder
	you, agent, idle, credited := 0, 0, 0, 0
	cuts, flags := map[string]int{}, map[string]int{}
	for _, m := range r.Minutes {
		switch m.Label {
		case LabelYou:
			you++
		case LabelAgent:
			agent++
		case LabelIdle:
			idle++
		}
		credited += m.Credited
		for _, c := range m.Cuts {
			cuts[c]++
		}
		for _, f := range m.Flags {
			flags[f]++
		}
	}
	counts := func(m map[string]int) string {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(m)) {
			parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
		}
		if len(parts) == 0 {
			return "-"
		}
		return strings.Join(parts, " ")
	}
	fmt.Fprintf(&b, "rules %s\n", r.RulesVersion)
	fmt.Fprintf(&b, "rows %d: you=%d agent=%d idle=%d; credited %ds (%.1f min)\n",
		len(r.Minutes), you, agent, idle, credited, float64(credited)/60)
	fmt.Fprintf(&b, "cuts: %s\nflags: %s\n\n", counts(cuts), counts(flags))
	for _, m := range r.Minutes {
		list := func(l []string) string {
			if len(l) == 0 {
				return "-"
			}
			return strings.Join(l, ",")
		}
		project := m.Project
		if project == "" {
			project = `""`
		}
		fmt.Fprintf(&b, "%s  %-5s %2d  %-8s cuts=%s flags=%s\n",
			m.Minute.Format("2006-01-02T15:04Z"), m.Label, m.Credited, project, list(m.Cuts), list(m.Flags))
	}
	return b.String()
}
