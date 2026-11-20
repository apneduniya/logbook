package gitwatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// commits returns the commit events in evs.
func commits(evs []wire.GitEvent) []wire.GitEvent {
	var out []wire.GitEvent
	for _, e := range evs {
		if e.Kind == "commit" {
			out = append(out, e)
		}
	}
	return out
}

// at returns env setting a commit's author and committer dates to d ago, so
// commits made within one test second get distinct author times.
func at(d time.Duration) []string {
	v := strconv.FormatInt(time.Now().Add(-d).Unix(), 10) + " +0000"
	return []string{"GIT_AUTHOR_DATE=" + v, "GIT_COMMITTER_DATE=" + v}
}

func TestRewrittenCommitsAreNotRecordedAgain(t *testing.T) {
	gitEnvForTests(t)

	t.Run("amend", func(t *testing.T) {
		dir := newRepo(t)
		commitFile(t, dir, "a.txt", "a\n", at(10*time.Minute)...)
		sink := &fakeSink{}
		w := newWatcher(staticRepos(Repo{Path: dir, Project: "p"}), mustRunner(t))
		w.Poll(t.Context(), sink)
		if n := len(commits(sink.take())); n != 1 {
			t.Fatalf("first poll recorded %d commits", n)
		}
		// Amending keeps the author time and makes a new hash.
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\nb\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, dir, nil, "commit", "-q", "-a", "--amend", "--no-edit")
		w.Poll(t.Context(), sink)
		if c := commits(sink.take()); len(c) != 0 {
			t.Fatalf("amend recorded again: %+v", c)
		}
		// Amending twice is still one commit.
		git(t, dir, nil, "commit", "-q", "--amend", "-m", "reworded")
		w.Poll(t.Context(), sink)
		if c := commits(sink.take()); len(c) != 0 {
			t.Fatalf("second amend recorded: %+v", c)
		}
		// A genuinely new commit afterwards is still recorded.
		h := commitFile(t, dir, "b.txt", "b\n")
		w.Poll(t.Context(), sink)
		if c := commits(sink.take()); len(c) != 1 || c[0].CommitHash != h {
			t.Fatalf("next commit = %+v", c)
		}
	})

	t.Run("rebase of recent work", func(t *testing.T) {
		dir := newRepo(t)
		commitFile(t, dir, "base.txt", "base\n", at(50*time.Minute)...)
		git(t, dir, nil, "switch", "-q", "-c", "topic")
		commitFile(t, dir, "t1.txt", "1\n", at(40*time.Minute)...)
		commitFile(t, dir, "t2.txt", "2\n", at(30*time.Minute)...)
		sink := &fakeSink{}
		w := newWatcher(staticRepos(Repo{Path: dir, Project: "p"}), mustRunner(t))
		w.Poll(t.Context(), sink)
		if n := len(commits(sink.take())); n != 3 {
			t.Fatalf("first poll recorded %d commits", n)
		}
		git(t, dir, nil, "switch", "-q", "main")
		mainCommit := commitFile(t, dir, "m.txt", "m\n", at(20*time.Minute)...)
		git(t, dir, nil, "switch", "-q", "topic")
		git(t, dir, nil, "rebase", "-q", "main")
		w.Poll(t.Context(), sink)
		// Only main's new commit is new work; t1 and t2 were rewritten.
		if c := commits(sink.take()); len(c) != 1 || c[0].CommitHash != mainCommit {
			t.Fatalf("after rebase = %+v", c)
		}
	})

	t.Run("rebase brings back old commits", func(t *testing.T) {
		dir := newRepo(t)
		commitFile(t, dir, "base.txt", "base\n", at(8*24*time.Hour)...)
		git(t, dir, nil, "switch", "-q", "-c", "topic")
		commitFile(t, dir, "old1.txt", "1\n", at(7*24*time.Hour)...)
		commitFile(t, dir, "old2.txt", "2\n", at(7*24*time.Hour-time.Minute)...)
		git(t, dir, nil, "switch", "-q", "main")
		mainCommit := commitFile(t, dir, "m.txt", "m\n")
		git(t, dir, nil, "switch", "-q", "topic")
		// The rebase gives the week-old commits fresh committer dates.
		git(t, dir, nil, "rebase", "-q", "main")
		sink := &fakeSink{}
		newWatcher(staticRepos(Repo{Path: dir, Project: "p"}), mustRunner(t)).Poll(t.Context(), sink)
		var hashes []string
		for _, c := range commits(sink.take()) {
			hashes = append(hashes, c.CommitHash)
		}
		// Only main's commit is recent; the rebased week-old ones are not,
		// even though the rebase gave them fresh committer dates.
		if len(hashes) != 1 || hashes[0] != mainCommit {
			t.Fatalf("recorded %v, want %s only", hashes, mainCommit)
		}
	})

	t.Run("same-second commits are both recorded", func(t *testing.T) {
		dir := newRepo(t)
		same := at(time.Minute)
		h1 := commitFile(t, dir, "a.txt", "a\n", same...)
		sink := &fakeSink{}
		w := newWatcher(staticRepos(Repo{Path: dir, Project: "p"}), mustRunner(t))
		w.Poll(t.Context(), sink)
		h2 := commitFile(t, dir, "b.txt", "b\n", same...)
		w.Poll(t.Context(), sink)
		c := commits(sink.take())
		if len(c) != 2 || c[0].CommitHash != h1 || c[1].CommitHash != h2 {
			t.Fatalf("commits = %+v", c)
		}
	})
}

// flakySink fails the first n writes, like a locked database.
type flakySink struct {
	fakeSink
	failures int
}

func (f *flakySink) RecordGit(ctx context.Context, g wire.GitEvent) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("database is locked")
	}
	return f.fakeSink.RecordGit(ctx, g)
}

func TestSinkFailureIsRetried(t *testing.T) {
	gitEnvForTests(t)
	dir := newRepo(t)
	h := commitFile(t, dir, "a.txt", "a\n")
	sink := &flakySink{failures: 2} // the commit and the worktree snapshot
	w := newWatcher(staticRepos(Repo{Path: dir, Project: "p"}), mustRunner(t))
	w.Poll(t.Context(), sink)
	if len(sink.events) != 0 {
		t.Fatalf("events while failing = %+v", sink.events)
	}
	w.Poll(t.Context(), sink)
	if ev := sink.take(); kinds(ev) != "commit,worktree" || ev[0].CommitHash != h {
		t.Fatalf("retry = %+v", ev)
	}
	w.Poll(t.Context(), sink)
	if ev := sink.take(); len(ev) != 0 {
		t.Fatalf("recorded twice: %+v", ev)
	}

	// Only the failed event is retried: here the worktree snapshot.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := commitFile(t, dir, "b.txt", "b\n")
	sink.failures = 0
	sink.fakeSink = fakeSink{}
	// Fail the second write (the worktree), not the first (the commit).
	w.Poll(t.Context(), &failNth{sink: &sink.fakeSink, n: 2})
	if c := sink.take(); kinds(c) != "commit" || c[0].CommitHash != h2 {
		t.Fatalf("partial poll = %+v", c)
	}
	w.Poll(t.Context(), sink)
	if ev := sink.take(); kinds(ev) != "worktree" || ev[0].FilesChanged != 1 {
		t.Fatalf("worktree retry = %+v", ev)
	}
}

// failNth fails only its nth RecordGit call.
type failNth struct {
	sink  *fakeSink
	n     int
	calls int
}

func (f *failNth) RecordActivity(context.Context, wire.ActivitySample) error { return nil }

func (f *failNth) RecordGit(ctx context.Context, g wire.GitEvent) error {
	f.calls++
	if f.calls == f.n {
		return errors.New("database is locked")
	}
	return f.sink.RecordGit(ctx, g)
}

func TestRepoFilterDriversAreNotRun(t *testing.T) {
	gitEnvForTests(t)
	if runtime.GOOS == "windows" {
		t.Skip("the filter command is a shell script")
	}
	dir := newRepo(t)
	commitFile(t, dir, "a.txt", "a\n")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	// What a hostile .git/config in a copied project could hold.
	git(t, dir, nil, "config", "filter.evil.clean", "sh -c 'touch "+marker+"; cat'")
	git(t, dir, nil, "config", "filter.evil.required", "true")
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("*.txt filter=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make a.txt stat-dirty with a new mtime, so git must read it.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), future, future); err != nil {
		t.Fatal(err)
	}

	sink := &fakeSink{}
	newWatcher(staticRepos(Repo{Path: dir, Project: "p"}), mustRunner(t)).Poll(t.Context(), sink)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repo's clean filter was run")
	}
	ev := sink.take()
	if kinds(ev) != "commit,worktree" || ev[1].Insertions != 1 {
		t.Fatalf("events = %+v", ev)
	}

	// Sanity check: plain git does run it, so the test is meaningful.
	git(t, dir, nil, "diff", "--shortstat", "HEAD")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: plain git didn't run the filter either: %v", err)
	}
}
