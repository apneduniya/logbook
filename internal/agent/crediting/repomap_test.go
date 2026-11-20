package crediting

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// repoList is a RepoSource whose answer the test changes.
type repoList struct {
	mu    sync.Mutex
	repos []struggle.Repo
}

func (r *repoList) set(repos ...struggle.Repo) {
	r.mu.Lock()
	r.repos = repos
	r.mu.Unlock()
}

func (r *repoList) ActiveRepos(context.Context) ([]struggle.Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]struggle.Repo(nil), r.repos...), nil
}

// projectOf is the project the engine's repo mapping gives /src/app.
func projectOf(in struggle.Input) string {
	for _, r := range in.Repos {
		if r.Path == "/src/app" {
			return r.Project
		}
	}
	return "unmapped"
}

func TestReposAtAndGroups(t *testing.T) {
	rows := []localdb.RepoMapping{
		{Since: at("11:00"), Path: "/src/app", Project: "old"},
		{Since: at("11:10"), Path: "/src/lib", Project: "lib"},
		{Since: at("12:31").Add(5 * time.Second), Path: "/src/app", Project: "new"},
	}
	tests := []struct {
		minute string
		want   []struggle.Repo
	}{
		{"10:30", nil}, // final at 11:00; a row recorded at 11:00 is not before that
		{"10:31", []struggle.Repo{{Path: "/src/app", Project: "old"}}},
		{"12:01", []struggle.Repo{{Path: "/src/app", Project: "old"}, {Path: "/src/lib", Project: "lib"}}}, // final at 12:31:00
		{"12:02", []struggle.Repo{{Path: "/src/app", Project: "new"}, {Path: "/src/lib", Project: "lib"}}},
	}
	for _, tt := range tests {
		if got := ReposAt(rows, at(tt.minute)); !slices.Equal(got, tt.want) {
			t.Errorf("ReposAt(%s) = %v, want %v", tt.minute, got, tt.want)
		}
	}
	groups := repoGroups(rows, at("11:31"), at("12:31"))
	if len(groups) != 2 || !groups[0].from.Equal(at("11:31")) || !groups[0].to.Equal(at("12:02")) ||
		!groups[1].from.Equal(at("12:02")) || !groups[1].to.Equal(at("12:31")) || groups[1].repos[0].Project != "new" {
		t.Fatalf("groups = %+v", groups)
	}
	if g := repoGroups(nil, at("11:31"), at("12:31")); len(g) != 1 {
		t.Fatalf("groups without a mapping = %+v", g)
	}
}

// A repo mapping that changes after a minute is final must not change that
// minute: it is computed with the mapping as it stood while the minute was
// open, so the agent never credits it again under another project, and a
// replay from the recorded mapping gives the same answer.
func TestRepoMappingChangeKeepsFinalMinutes(t *testing.T) {
	e := newEnv(t, at("12:01").Add(5*time.Second))
	repos := &repoList{}
	repos.set(struggle.Repo{Path: "/src/app", Project: "old"})
	busy := map[time.Time]bool{at("12:00"): true, at("12:30"): true}
	e.options = func(o *Options) {
		o.Repos = repos
		o.Compute = func(in struggle.Input) struggle.Result {
			e.mu.Lock()
			e.inputs = append(e.inputs, in)
			e.mu.Unlock()
			var out []struggle.Minute
			for m := range busy {
				if !m.Before(in.Window.From) && m.Before(in.Window.To) {
					out = append(out, scripted(m, projectOf(in), 60))
				}
			}
			return struggle.Result{RulesVersion: struggle.RulesVersion, Minutes: out}
		}
	}
	e.open()
	e.run()
	e.wantServerMinute(at("12:00"), "old", 60)

	// 12:31: the repo is now called "new"; 12:00 is final.
	repos.set(struggle.Repo{Path: "/src/app", Project: "new"})
	e.mu.Lock()
	e.now = at("12:31").Add(5 * time.Second)
	e.inputs = nil
	e.mu.Unlock()
	e.run()
	e.wantServerMinute(at("12:30"), "new", 60)
	if _, ok := e.srv.minute(e.deviceID(), at("12:00"), "new"); ok {
		t.Fatal("final minute 12:00 credited again under the new mapping")
	}
	if s := e.status(); s.Minutes.Rejected != 0 || s.Minutes.Waiting != 0 {
		t.Fatalf("status = %+v", s.Minutes)
	}
	// Every engine run covers the whole window (only its mapping differs),
	// and 12:00 is taken from the one with the mapping it had while open.
	e.wantServerMinute(at("12:00"), "old", 60)
}
