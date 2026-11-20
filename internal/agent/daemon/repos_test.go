package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
)

type fakeDirs []localdb.DirSeen

func (f fakeDirs) RecentDirs(context.Context, time.Time, int) ([]localdb.DirSeen, error) {
	return f, nil
}

func TestActiveRepos(t *testing.T) {
	root := t.TempDir() // absolute on every OS; nothing is created under it
	p := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	gitDirs := map[string]bool{
		p("app", ".git"):           true,
		p("app", "vendor", ".git"): true, // nested repo (submodule)
		p("other", ".git"):         true,
	}
	var stats int
	exists := func(path string) bool { stats++; return gitDirs[path] }

	dirs := fakeDirs{
		{Dir: p("app", "src", "pkg"), Project: ""},
		{Dir: p("app", "cmd"), Project: "my-app"},
		{Dir: p("app", "vendor", "lib")},
		{Dir: p("notes")},
		{Dir: "relative/dir"},
		{Dir: p("other"), Project: "other-proj"},
	}
	r := NewRepos(dirs, exists)
	got, err := r.ActiveRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Repo{
		{Path: p("app"), Project: "my-app"},
		{Path: p("app", "vendor"), Project: "vendor"},
		{Path: p("other"), Project: "other-proj"},
	}
	if len(got) != len(want) {
		t.Fatalf("repos = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("repo %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	before := stats
	if _, err := r.ActiveRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats != before {
		t.Fatalf("git roots not cached: %d extra stats", stats-before)
	}
}
