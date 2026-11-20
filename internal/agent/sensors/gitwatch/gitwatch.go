package gitwatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// Defaults (ADR 0009).
const (
	DefaultInterval   = 60 * time.Second
	DefaultGitTimeout = 5 * time.Second
	// lookback is how far back commits are listed on each poll. Hashes seen
	// within it are remembered, so each commit is emitted once per process.
	lookback = time.Hour
	// maxCommitsPerPoll bounds one git log; more than this in an hour is a
	// rebase or import, not hand-written commits.
	maxCommitsPerPoll = 200
	// maxRepos bounds the per-repo state.
	maxRepos = 256
	// failBackoff is how long a repo is skipped after git fails or times out
	// on it, so a huge repo doesn't cost 5 s of CPU every minute.
	failBackoff = 10 * time.Minute
	// forgetAfter drops state for repos that stopped being active.
	forgetAfter = 2 * time.Hour
)

// Repo is a repository with recent activity. It has the same fields as the
// daemon's Repo; see RepoSourceFunc for adapting a daemon RepoSource.
type Repo struct {
	Path    string
	Project string
}

// RepoSource lists the repos with activity in the last hour.
type RepoSource interface {
	ActiveRepos(ctx context.Context) ([]Repo, error)
}

// RepoSourceFunc adapts a function to RepoSource.
type RepoSourceFunc func(ctx context.Context) ([]Repo, error)

// ActiveRepos calls f.
func (f RepoSourceFunc) ActiveRepos(ctx context.Context) ([]Repo, error) { return f(ctx) }

// Sink receives git events. It has the daemon Sink's method set.
type Sink = interface {
	RecordActivity(ctx context.Context, s wire.ActivitySample) error
	RecordGit(ctx context.Context, g wire.GitEvent) error
}

// Watcher is the git sensor. Create it with New; Run it once.
type Watcher struct {
	repos    RepoSource
	runner   Runner // nil when git is not installed
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration
	timeout  time.Duration
	lookPath func(string) (string, error)

	state map[string]*repoState // keyed by cleaned repo path
}

// repoState is what the watcher remembers about one repository.
type repoState struct {
	seen         map[string]time.Time // commit hash → when this process first saw it
	worktree     *diffStat            // last snapshot; nil before the first
	skipUntil    time.Time            // backoff after a failure
	lastActive   time.Time
	failing      bool // a failure was logged and not yet followed by success
	warnedNoUser bool
}

// Option configures a Watcher.
type Option func(*Watcher)

// WithLogger sets the logger (default: discard).
func WithLogger(l *slog.Logger) Option { return func(w *Watcher) { w.log = l } }

// WithClock sets the time source.
func WithClock(now func() time.Time) Option { return func(w *Watcher) { w.now = now } }

// WithInterval sets the poll interval.
func WithInterval(d time.Duration) Option { return func(w *Watcher) { w.interval = d } }

// WithGitTimeout sets the timeout for each git process.
func WithGitTimeout(d time.Duration) Option { return func(w *Watcher) { w.timeout = d } }

// WithRunner replaces the git runner (tests use a fake).
func WithRunner(r Runner) Option { return func(w *Watcher) { w.runner = r } }

// WithLookPath replaces exec.LookPath, used to find git.
func WithLookPath(f func(string) (string, error)) Option {
	return func(w *Watcher) { w.lookPath = f }
}

// New returns a git watcher over repos.
func New(repos RepoSource, opts ...Option) *Watcher {
	w := &Watcher{
		repos:    repos,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:      time.Now,
		interval: DefaultInterval,
		timeout:  DefaultGitTimeout,
		lookPath: exec.LookPath,
		state:    map[string]*repoState{},
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Name identifies the sensor in daemon logs and status.
func (w *Watcher) Name() string { return "git" }

// Run polls until ctx is done, then returns nil. If git is not installed it
// logs once and idles until ctx is done.
func (w *Watcher) Run(ctx context.Context, sink Sink) error {
	if w.runner == nil {
		path, err := w.lookPath("git")
		if err != nil {
			w.log.Warn("git not found on PATH; git activity will not be recorded", "err", err)
			<-ctx.Done()
			return nil
		}
		w.runner = ExecRunner{Path: path, Timeout: w.timeout}
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		w.Poll(ctx, sink)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Poll checks every active repo once. Errors are logged, never returned:
// one broken repo must not stop the others.
func (w *Watcher) Poll(ctx context.Context, sink Sink) {
	repos, err := w.repos.ActiveRepos(ctx)
	if err != nil {
		w.log.Warn("listing active repos", "err", err)
		return
	}
	now := w.now()
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		w.pollRepo(ctx, sink, r, now)
	}
	w.forget(now)
}

func (w *Watcher) pollRepo(ctx context.Context, sink Sink, r Repo, now time.Time) {
	if r.Path == "" || !filepath.IsAbs(r.Path) || strings.TrimSpace(r.Project) == "" {
		return // only absolute paths: a relative one would resolve against the daemon's cwd
	}
	key := filepath.Clean(r.Path)
	st := w.state[key]
	if st == nil {
		if len(w.state) >= maxRepos {
			return
		}
		st = &repoState{seen: map[string]time.Time{}}
		w.state[key] = st
	}
	st.lastActive = now
	if now.Before(st.skipUntil) {
		return
	}
	events, err := w.scan(ctx, key, r.Project, st, now)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		st.skipUntil = now.Add(failBackoff)
		if !st.failing {
			st.failing = true
			w.log.Warn("git sensor skipping repo for a while", "project", r.Project, "err", err,
				"retry_in", failBackoff)
		}
		return
	}
	if st.failing {
		st.failing = false
		w.log.Info("git sensor repo recovered", "project", r.Project)
	}
	for _, ev := range events {
		if err := sink.RecordGit(ctx, ev); err != nil {
			w.log.Warn("recording git event", "project", r.Project, "err", err)
		}
	}
}

// scan runs git for one repo and returns the events to record. State is
// only updated once every git call has succeeded, so a failed poll is
// retried in full later.
func (w *Watcher) scan(ctx context.Context, dir, project string, st *repoState, now time.Time) ([]wire.GitEvent, error) {
	head, err := w.runner.Git(ctx, dir, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	if exitCode(err) == 1 && ctx.Err() == nil {
		return nil, nil // unborn branch (fresh `git init`): nothing to report yet
	}
	if err != nil {
		return nil, err
	}
	if !isHash(strings.TrimSpace(string(head))) {
		return nil, errors.New("unexpected rev-parse output")
	}

	commits, err := w.newCommits(ctx, dir, project, st, now)
	if err != nil {
		return nil, err
	}

	stat, err := w.runner.Git(ctx, dir, "diff", "--shortstat", "--no-ext-diff", "--no-textconv",
		"--no-renames", "HEAD", "--")
	if err != nil {
		return nil, err
	}
	snap := parseShortstat(string(stat))

	// Commit only now that every call succeeded.
	var events []wire.GitEvent
	for _, c := range commits {
		st.seen[c.Hash] = now
		events = append(events, wire.GitEvent{
			Time: c.AuthorTime, Project: project, Kind: "commit", CommitHash: c.Hash,
			FilesChanged: c.Stat.Files, Insertions: c.Stat.Insertions, Deletions: c.Stat.Deletions,
		})
	}
	if st.worktree == nil || *st.worktree != snap {
		st.worktree = &snap
		events = append(events, wire.GitEvent{
			Time: now.UTC(), Project: project, Kind: "worktree",
			FilesChanged: snap.Files, Insertions: snap.Insertions, Deletions: snap.Deletions,
		})
	}
	// git log lists only commits whose committer date is within lookback,
	// so a hash first seen more than 2*lookback ago can't come back.
	for h, t := range st.seen {
		if now.Sub(t) > 2*lookback {
			delete(st.seen, h)
		}
	}
	return events, nil
}

// newCommits lists HEAD's commits from the last hour, authored by the repo's
// user.email, that this process has not emitted yet.
func (w *Watcher) newCommits(ctx context.Context, dir, project string, st *repoState, now time.Time) ([]commit, error) {
	email, err := w.userEmail(ctx, dir)
	if err != nil {
		return nil, err
	}
	if email == "" {
		if !st.warnedNoUser {
			st.warnedNoUser = true
			w.log.Info("git user.email is not set for repo; its commits are not recorded", "project", project)
		}
		return nil, nil
	}
	st.warnedNoUser = false
	out, err := w.runner.Git(ctx, dir, "log", "HEAD", logFormat, "--shortstat",
		"--no-ext-diff", "--no-textconv", "--no-renames", "--no-show-signature",
		"--since="+strconv.FormatInt(now.Add(-lookback).Unix(), 10),
		"--max-count="+strconv.Itoa(maxCommitsPerPoll), "--")
	if err != nil {
		return nil, err
	}
	all, err := parseLog(out)
	if err != nil {
		return nil, err
	}
	var mine []commit
	for _, c := range all {
		if _, dup := st.seen[c.Hash]; dup || !sameEmail(c.AuthorEmail, email) {
			continue
		}
		mine = append(mine, c)
	}
	// git log lists newest first; emit oldest first.
	for i, j := 0, len(mine)-1; i < j; i, j = i+1, j-1 {
		mine[i], mine[j] = mine[j], mine[i]
	}
	return mine, nil
}

// userEmail returns the repo's effective user.email, or "" when unset.
func (w *Watcher) userEmail(ctx context.Context, dir string) (string, error) {
	out, err := w.runner.Git(ctx, dir, "config", "--get", "user.email")
	if exitCode(err) == 1 && ctx.Err() == nil {
		return "", nil // key not set
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// exitCode returns the exit code carried by err (an *exec.ExitError, or a
// fake's error with the same method), or -1.
func exitCode(err error) int {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

// forget drops state for repos that have not been active for a while.
func (w *Watcher) forget(now time.Time) {
	for k, st := range w.state {
		if now.Sub(st.lastActive) > forgetAfter {
			delete(w.state, k)
		}
	}
}
