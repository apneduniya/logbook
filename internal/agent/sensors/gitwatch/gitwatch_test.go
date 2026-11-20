package gitwatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

const me = "me@example.com"

type fakeSink struct{ events []wire.GitEvent }

func (f *fakeSink) RecordActivity(context.Context, wire.ActivitySample) error { return nil }

func (f *fakeSink) RecordGit(_ context.Context, g wire.GitEvent) error {
	if err := g.Validate(); err != nil {
		return err
	}
	f.events = append(f.events, g)
	return nil
}

func (f *fakeSink) take() []wire.GitEvent {
	ev := f.events
	f.events = nil
	return ev
}

func staticRepos(repos ...Repo) RepoSource {
	return RepoSourceFunc(func(context.Context) ([]Repo, error) { return repos, nil })
}

// gitEnvForTests isolates git from the developer's global config.
func gitEnvForTests(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
}

// git runs a setup command in dir and fails the test on error.
func git(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", "-b", "main")
	git(t, dir, nil, "config", "user.email", me)
	git(t, dir, nil, "config", "user.name", "Me")
	git(t, dir, nil, "config", "commit.gpgsign", "false")
	return dir
}

// commitFile writes content to name and commits it; env can override author
// and dates. Returns the new HEAD hash.
func commitFile(t *testing.T, dir, name, content string, env ...string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, nil, "add", name)
	git(t, dir, env, "commit", "-q", "-m", "change "+name)
	return git(t, dir, nil, "rev-parse", "HEAD")
}

func newWatcher(repos RepoSource, opts ...Option) *Watcher {
	return New(repos, append([]Option{WithLogger(slog.New(slog.DiscardHandler))}, opts...)...)
}

func mustRunner(t *testing.T) Option {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	return WithRunner(ExecRunner{Path: path, Timeout: 10 * time.Second})
}

func kinds(evs []wire.GitEvent) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Kind)
	}
	return strings.Join(s, ",")
}

func TestCommitsAndWorktree(t *testing.T) {
	gitEnvForTests(t)
	dir := newRepo(t)
	old := strconv.FormatInt(time.Now().Add(-3*time.Hour).Unix(), 10) + " +0000"
	commitFile(t, dir, "old.txt", "old\n", "GIT_AUTHOR_DATE="+old, "GIT_COMMITTER_DATE="+old)
	mine := commitFile(t, dir, "a.txt", "one\ntwo\n")
	commitFile(t, dir, "b.txt", "theirs\n", "GIT_AUTHOR_EMAIL=someone@else.org", "GIT_AUTHOR_NAME=Someone")

	sink := &fakeSink{}
	w := newWatcher(staticRepos(Repo{Path: dir, Project: "demo"}), mustRunner(t))

	// First poll: my recent commit only (not the 3-hour-old one, not the
	// other author's), plus the initial clean worktree snapshot.
	w.Poll(t.Context(), sink)
	ev := sink.take()
	if kinds(ev) != "commit,worktree" {
		t.Fatalf("first poll = %+v", ev)
	}
	c := ev[0]
	if c.CommitHash != mine || c.Project != "demo" || c.FilesChanged != 1 || c.Insertions != 2 || c.Deletions != 0 {
		t.Fatalf("commit event = %+v", c)
	}
	if ev[1].FilesChanged != 0 || ev[1].Insertions != 0 {
		t.Fatalf("clean worktree event = %+v", ev[1])
	}

	// Nothing changed: nothing emitted.
	w.Poll(t.Context(), sink)
	if ev := sink.take(); len(ev) != 0 {
		t.Fatalf("idle poll emitted %+v", ev)
	}

	// Uncommitted edits: one worktree event with the diff size.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nTWO\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.Poll(t.Context(), sink)
	ev = sink.take()
	if kinds(ev) != "worktree" || ev[0].FilesChanged != 1 || ev[0].Insertions != 2 || ev[0].Deletions != 1 {
		t.Fatalf("worktree poll = %+v", ev)
	}

	// Committing them: a commit event, and the worktree back to clean.
	next := commitFile(t, dir, "a.txt", "one\nTWO\nthree\n")
	w.Poll(t.Context(), sink)
	ev = sink.take()
	if kinds(ev) != "commit,worktree" || ev[0].CommitHash != next || ev[1].Insertions != 0 {
		t.Fatalf("after commit = %+v", ev)
	}

	// A new watcher (daemon restart) re-emits the last hour's commits; the
	// daemon dedups them by hash.
	w2 := newWatcher(staticRepos(Repo{Path: dir, Project: "demo"}), mustRunner(t))
	w2.Poll(t.Context(), sink)
	if ev := sink.take(); kinds(ev) != "commit,commit,worktree" || ev[0].CommitHash != mine || ev[1].CommitHash != next {
		t.Fatalf("restart poll = %+v", ev)
	}
}

func TestEdgeCaseRepos(t *testing.T) {
	gitEnvForTests(t)

	t.Run("unborn branch", func(t *testing.T) {
		dir := newRepo(t)
		sink := &fakeSink{}
		w := newWatcher(staticRepos(Repo{Path: dir, Project: "fresh"}), mustRunner(t))
		w.Poll(t.Context(), sink)
		if len(sink.events) != 0 {
			t.Fatalf("events = %+v", sink.events)
		}
		if st := w.state[filepath.Clean(dir)]; st == nil || !st.skipUntil.IsZero() {
			t.Fatal("unborn repo was treated as a failure")
		}
		// The first commit shows up on the next poll.
		commitFile(t, dir, "a.txt", "a\n")
		w.Poll(t.Context(), sink)
		if kinds(sink.events) != "commit,worktree" {
			t.Fatalf("after first commit = %+v", sink.events)
		}
	})

	t.Run("detached head", func(t *testing.T) {
		dir := newRepo(t)
		commitFile(t, dir, "a.txt", "a\n")
		git(t, dir, nil, "checkout", "-q", "--detach")
		h := commitFile(t, dir, "b.txt", "b\n")
		sink := &fakeSink{}
		newWatcher(staticRepos(Repo{Path: dir, Project: "det"}), mustRunner(t)).Poll(t.Context(), sink)
		if kinds(sink.events) != "commit,commit,worktree" || sink.events[1].CommitHash != h {
			t.Fatalf("events = %+v", sink.events)
		}
	})

	t.Run("shallow clone", func(t *testing.T) {
		src := newRepo(t)
		commitFile(t, src, "a.txt", "a\n")
		commitFile(t, src, "b.txt", "b\n")
		dst := filepath.Join(t.TempDir(), "clone")
		u := filepath.ToSlash(src)
		if !strings.HasPrefix(u, "/") {
			u = "/" + u // Windows: file:///C:/...
		}
		git(t, t.TempDir(), nil, "clone", "-q", "--depth", "1", "file://"+u, dst)
		git(t, dst, nil, "config", "user.email", me)
		git(t, dst, nil, "config", "user.name", "Me")
		sink := &fakeSink{}
		w := newWatcher(staticRepos(Repo{Path: dst, Project: "shallow"}), mustRunner(t))
		w.Poll(t.Context(), sink)
		// The boundary commit has no parents in the clone, so git would
		// report the whole tree (a.txt and b.txt) as its diff. It came
		// from the fetch, not from work here: skipped, not recorded.
		if kinds(sink.events) != "worktree" {
			t.Fatalf("events = %+v", sink.events)
		}
		// Work done in the clone is recorded with its own stats.
		sink.take()
		h := commitFile(t, dst, "c.txt", "c1\nc2\nc3\n")
		w.Poll(t.Context(), sink)
		ev := sink.take()
		if kinds(ev) != "commit" || ev[0].CommitHash != h || ev[0].FilesChanged != 1 || ev[0].Insertions != 3 {
			t.Fatalf("commit in shallow clone = %+v", ev)
		}
	})

	t.Run("no user.email", func(t *testing.T) {
		dir := newRepo(t)
		commitFile(t, dir, "a.txt", "a\n")
		git(t, dir, nil, "config", "--unset", "user.email")
		sink := &fakeSink{}
		newWatcher(staticRepos(Repo{Path: dir, Project: "anon"}), mustRunner(t)).Poll(t.Context(), sink)
		if kinds(sink.events) != "worktree" {
			t.Fatalf("events = %+v", sink.events)
		}
	})

	t.Run("root commit of a full repo is recorded", func(t *testing.T) {
		dir := newRepo(t)
		h := commitFile(t, dir, "a.txt", "a\nb\n")
		sink := &fakeSink{}
		newWatcher(staticRepos(Repo{Path: dir, Project: "root"}), mustRunner(t)).Poll(t.Context(), sink)
		if kinds(sink.events) != "commit,worktree" || sink.events[0].CommitHash != h || sink.events[0].Insertions != 2 {
			t.Fatalf("events = %+v", sink.events)
		}
	})

	t.Run("not a repo backs off", func(t *testing.T) {
		dir := t.TempDir()
		sink := &fakeSink{}
		w := newWatcher(staticRepos(Repo{Path: dir, Project: "x"}), mustRunner(t))
		w.Poll(t.Context(), sink)
		if len(sink.events) != 0 || w.state[filepath.Clean(dir)].skipUntil.IsZero() {
			t.Fatalf("events=%v state=%+v", sink.events, w.state[filepath.Clean(dir)])
		}
	})

	t.Run("relative and empty paths are ignored", func(t *testing.T) {
		sink := &fakeSink{}
		w := newWatcher(staticRepos(Repo{Path: "rel/path", Project: "x"}, Repo{Path: "", Project: "y"}), mustRunner(t))
		w.Poll(t.Context(), sink)
		if len(sink.events) != 0 || len(w.state) != 0 {
			t.Fatalf("events=%v state=%d", sink.events, len(w.state))
		}
	})
}

// slowGit is a fake git that never answers in time.
func slowGit(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake slow git is a shell script")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	return path
}

func TestTimeoutBacksOff(t *testing.T) {
	path := slowGit(t)
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	calls := 0
	w := newWatcher(
		RepoSourceFunc(func(context.Context) ([]Repo, error) {
			calls++
			return []Repo{{Path: dir, Project: "huge"}}, nil
		}),
		WithRunner(ExecRunner{Path: path, Timeout: 200 * time.Millisecond}),
		WithClock(func() time.Time { return now }),
	)

	start := time.Now()
	sink := &fakeSink{}
	w.Poll(t.Context(), sink)
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("poll took %v despite a 200ms timeout", el)
	}
	st := w.state[filepath.Clean(dir)]
	if st == nil || !st.skipUntil.Equal(now.Add(failBackoff)) {
		t.Fatalf("repo not backed off: %+v", st)
	}

	// While backed off, git is not run at all.
	start = time.Now()
	w.Poll(t.Context(), sink)
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("backed-off poll ran git (%v)", el)
	}
	if len(sink.events) != 0 || calls != 2 {
		t.Fatalf("events=%v calls=%d", sink.events, calls)
	}
}

func TestExecRunnerTimeoutError(t *testing.T) {
	r := ExecRunner{Path: slowGit(t), Timeout: 100 * time.Millisecond}
	_, err := r.Git(t.Context(), t.TempDir(), "status")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestGitMissing(t *testing.T) {
	w := newWatcher(staticRepos(), WithLookPath(func(string) (string, error) { return "", exec.ErrNotFound }))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	sink := &fakeSink{}
	if err := w.Run(ctx, sink); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if len(sink.events) != 0 {
		t.Fatalf("events = %v", sink.events)
	}
}

// fakeRunner answers git commands from a table, to test logic without git.
type fakeRunner struct {
	answers map[string]fakeAnswer
	calls   []string
}

type fakeAnswer struct {
	out string
	err error
}

type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func (f *fakeRunner) Git(_ context.Context, _ string, args ...string) ([]byte, error) {
	sub := subcommand(args)
	f.calls = append(f.calls, sub)
	a := f.answers[sub]
	return []byte(a.out), a.err
}

func TestFailedPollRetriesFully(t *testing.T) {
	h := strings.Repeat("c", 40)
	now := time.Now()
	r := &fakeRunner{answers: map[string]fakeAnswer{
		"rev-parse": {out: h + "\n"},
		"config":    {out: "user.email\n" + me + "\x00"},
		"log": {out: recordSep + h + fieldSep + strconv.FormatInt(now.Unix(), 10) + fieldSep + me + fieldSep +
			strings.Repeat("d", 40) + "\n\n 1 file changed, 3 insertions(+)\n"},
		"diff": {err: errors.New("index locked")},
	}}
	clock := now
	dir := t.TempDir()
	w := newWatcher(staticRepos(Repo{Path: dir, Project: "p"}), WithRunner(r), WithClock(func() time.Time { return clock }))
	sink := &fakeSink{}
	w.Poll(t.Context(), sink)
	if len(sink.events) != 0 {
		t.Fatalf("partial poll emitted %+v", sink.events)
	}
	// After the backoff the commit is still new, because state wasn't updated.
	r.answers["diff"] = fakeAnswer{}
	clock = clock.Add(failBackoff)
	w.Poll(t.Context(), sink)
	if kinds(sink.events) != "commit,worktree" || sink.events[0].Insertions != 3 {
		t.Fatalf("retry = %+v", sink.events)
	}
}

func TestStateIsBounded(t *testing.T) {
	r := &fakeRunner{answers: map[string]fakeAnswer{"rev-parse": {err: exitErr(1)}}}
	var repos []Repo
	base := t.TempDir()
	for i := range maxRepos + 10 {
		repos = append(repos, Repo{Path: filepath.Join(base, strconv.Itoa(i)), Project: "p"})
	}
	clock := time.Now()
	w := newWatcher(staticRepos(repos...), WithRunner(r), WithClock(func() time.Time { return clock }))
	w.Poll(t.Context(), &fakeSink{})
	if len(w.state) != maxRepos {
		t.Fatalf("state has %d repos, want %d", len(w.state), maxRepos)
	}
	// Repos that stop being active are forgotten.
	w.repos = staticRepos()
	clock = clock.Add(forgetAfter + time.Minute)
	w.Poll(t.Context(), &fakeSink{})
	if len(w.state) != 0 {
		t.Fatalf("state has %d repos after they went inactive", len(w.state))
	}
}

// The watcher must satisfy the daemon's Sensor contract by method set.
var _ interface {
	Name() string
	Run(ctx context.Context, sink Sink) error
} = (*Watcher)(nil)
