package struggle

import (
	"fmt"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// presentMinute is an Input whose window is the single minute t0, with the
// person typing in the editor throughout and one human write heartbeat per
// project name, a second apart.
func presentMinute(projects ...string) Input {
	in := Input{Window: Window{From: t0, To: at(time.Minute)}}
	for x := at(-5 * time.Minute); x.Before(at(5 * time.Minute)); x = x.Add(5 * time.Second) {
		in.Activity = append(in.Activity, wire.ActivitySample{Time: x, IdleSeconds: 1, FocusKind: "editor"})
	}
	for i, p := range projects {
		in.Heartbeats = append(in.Heartbeats, Heartbeat{
			Time: at(time.Duration(i+1) * time.Second), Project: p, Entity: fmt.Sprintf("/f%d.go", i), IsWrite: true,
		})
	}
	return in
}

func credit(res Result) (total int, projects []string) {
	for _, m := range res.Minutes {
		total += m.Credited
		projects = append(projects, m.Project)
	}
	return total, projects
}

// Project names leave the engine in the server's form (pkg/projectname), so
// two spellings of one project are one row and the server never rejects one
// of them as a duplicate.
func TestProjectNamesAreNormalized(t *testing.T) {
	tests := []struct {
		name     string
		in       Input
		projects []string
	}{
		{"spaces", presentMinute("app", " app "), []string{"app"}},
		{"control characters", presentMinute("app", "a\tpp\x00"), []string{"app"}},
		{"placeholder is no project", presentMinute("app", "<<LAST_PROJECT>>"), []string{"app"}},
		{"repo project", func() Input {
			in := presentMinute("app")
			in.Heartbeats[0].Entity = "/home/sam/app/main.go"
			in.Repos = []Repo{{Path: "/home/sam/app", Project: " app\n"}}
			return in
		}(), []string{"app"}},
		{"blank repo project falls back", func() Input {
			in := presentMinute("app")
			in.Heartbeats[0].Entity = "/home/sam/app/main.go"
			in.Repos = []Repo{{Path: "/home/sam/app", Project: "  "}}
			return in
		}(), []string{"app"}},
		{"ai event project", func() Input {
			in := presentMinute()
			in.AI = []wire.AIEvent{{Time: at(5 * time.Second), Kind: wire.AIPromptSubmit, Harness: wire.HarnessClaudeCode, SessionID: "s", Project: " app"}}
			return in
		}(), []string{"app"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, projects := credit(Compute(tt.in))
			if total != 60 || fmt.Sprint(projects) != fmt.Sprint(tt.projects) {
				t.Fatalf("credited %d s to %q, want 60 s to %q", total, projects, tt.projects)
			}
		})
	}
}

// A minute never has more rows than the server stores per device-minute
// (MaxProjectsPerMinute), and capping keeps the minute's credit.
func TestProjectsPerMinuteAreCapped(t *testing.T) {
	tests := []struct {
		name     string
		projects []string
		want     map[string]int
	}{
		{"nine equal projects", []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"},
			// The 8 smallest names win the tie and share the minute.
			map[string]int{"p1": 8, "p2": 8, "p3": 8, "p4": 8, "p5": 7, "p6": 7, "p7": 7, "p8": 7}},
		{"the smallest shares go", []string{
			"a", "a", "a", "b", "b", "c", "c", "d", "d", "e", "e", "f", "f", "g", "g", "h", "i", "j",
		}, // h, i and j tie for the last place; h wins it by name.
			map[string]int{"a": 11, "b": 8, "c": 8, "d": 8, "e": 7, "f": 7, "g": 7, "h": 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Compute(presentMinute(tt.projects...))
			got := map[string]int{}
			total := 0
			for _, m := range res.Minutes {
				got[m.Project] = m.Credited
				total += m.Credited
			}
			if len(res.Minutes) > MaxProjectsPerMinute || total != 60 || fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("got %d rows %v (%d s), want %v (60 s)", len(res.Minutes), got, total, tt.want)
			}
		})
	}
}
