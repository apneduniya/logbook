package gitwatch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// Defaults (ADR 0009).
const (
	DefaultInterval   = 60 * time.Second
	DefaultGitTimeout = 5 * time.Second
	// lookback is how far back commits are listed on each poll, by author
	// time. Commits handled within it are remembered, so each is emitted
	// once per process.
	lookback = time.Hour
	// maxSeen bounds the remembered commits per repo.
	maxSeen = 2 * maxCommitsPerPoll
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
	seen         map[string]seenCommit // commits recorded (or deliberately skipped), by hash
	worktree     *diffStat             // last recorded snapshot; nil before the first
	skipUntil    time.Time             // backoff after a failure
	lastActive   time.Time
	failing      bool // a failure was logged and not yet followed by success
	warnedNoUser bool
}

// seenCommit is what is remembered about a handled commit.
type seenCommit struct {
	author   time.Time // author time, which amend and rebase keep
	replaced bool      // a rewrite of it has been seen; it can't match another
}

// rewriteOf returns the handled commit that c rewrites (amend, rebase,
// cherry-pick): same author time, no longer on HEAD, and not already matched
// to another rewrite (in state, or in taken during this scan). onHead holds
// the hashes the current git log listed.
func (st *repoState) rewriteOf(c commit, onHead, taken map[string]bool) (string, bool) {
	for h, s := range st.seen {
		if s.author.Equal(c.AuthorTime) && !s.replaced && !onHead[h] && !taken[h] {
			return h, true
		}
	}
	return "", false
}

// expire forgets commits whose author time is well outside lookback (git
// log can't return them any more), then the oldest beyond maxSeen.
func (st *repoState) expire(now time.Time) {
	for h, s := range st.seen {
		if now.Sub(s.author) > 2*lookback {
			delete(st.seen, h)
		}
	}
	if len(st.seen) <= maxSeen {
		return
	}
	hashes := slices.Collect(maps.Keys(st.seen))
	slices.SortFunc(hashes, func(a, b string) int { return st.seen[a].author.Compare(st.seen[b].author) })
	for _, h := range hashes[:len(hashes)-maxSeen] {
		delete(st.seen, h)
	}
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
		st = &repoState{seen: map[string]seenCommit{}}
		w.state[key] = st
	}
	st.lastActive = now
	if now.Before(st.skipUntil) {
		return
	}
	res, err := w.scan(ctx, key, r.Project, st, now)
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
	w.record(ctx, sink, r.Project, st, res, now)
	st.expire(now)
}

// record hands the scan's events to the sink. State changes for an event
// (its hash seen, the worktree snapshot stored) are applied only after the
// sink accepted it, so an event the sink rejected (a locked database, a
// daemon shutting down) is offered again on the next poll.
func (w *Watcher) record(ctx context.Context, sink Sink, project string, st *repoState, res scanResult, now time.Time) {
	for _, c := range res.skip {
		st.seen[c.Hash] = seenCommit{author: c.AuthorTime}
	}
	for _, h := range res.replaced {
		if s, ok := st.seen[h]; ok {
			s.replaced = true
			st.seen[h] = s
		}
	}
	for _, c := range res.commits {
		if ctx.Err() != nil {
			return
		}
		err := sink.RecordGit(ctx, wire.GitEvent{
			Time: c.AuthorTime, Project: project, Kind: "commit", CommitHash: c.Hash,
			FilesChanged: c.Stat.Files, Insertions: c.Stat.Insertions, Deletions: c.Stat.Deletions,
		})
		if err != nil {
			w.log.Warn("recording git commit; will retry next poll", "project", project, "err", err)
			continue
		}
		st.seen[c.Hash] = seenCommit{author: c.AuthorTime}
	}
	if res.worktree == nil || ctx.Err() != nil {
		return
	}
	snap := *res.worktree
	err := sink.RecordGit(ctx, wire.GitEvent{
		Time: now.UTC(), Project: project, Kind: "worktree",
		FilesChanged: snap.Files, Insertions: snap.Insertions, Deletions: snap.Deletions,
	})
	if err != nil {
		w.log.Warn("recording git worktree; will retry next poll", "project", project, "err", err)
		return
	}
	st.worktree = &snap
}

// scanResult is what one scan found. Nothing in it has been applied to the
// repo's state yet.
type scanResult struct {
	commits  []commit  // new commits to record, oldest first
	skip     []commit  // commits to remember without recording (rewrites, shallow boundaries)
	replaced []string  // hashes of handled commits that a skipped rewrite replaces
	worktree *diffStat // new worktree snapshot, nil when unchanged
}

// scan runs git for one repo. It changes no state, so a failed scan is
// retried in full later.
func (w *Watcher) scan(ctx context.Context, dir, project string, st *repoState, now time.Time) (scanResult, error) {
	var res scanResult
	head, err := w.runner.Git(ctx, dir, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	if exitCode(err) == 1 && ctx.Err() == nil {
		return res, nil // unborn branch (fresh `git init`): nothing to report yet
	}
	if err != nil {
		return res, err
	}
	if !isHash(strings.TrimSpace(string(head))) {
		return res, errors.New("unexpected rev-parse output")
	}

	cfg, err := w.repoConfig(ctx, dir)
	if err != nil {
		return res, err
	}
	if cfg.email == "" {
		if !st.warnedNoUser {
			st.warnedNoUser = true
			w.log.Info("git user.email is not set for repo; its commits are not recorded", "project", project)
		}
	} else {
		st.warnedNoUser = false
		if err := w.newCommits(ctx, dir, cfg.email, st, now, &res); err != nil {
			return res, err
		}
	}

	// git diff runs the repo's clean filters on stat-dirty files; turn them
	// all off (see repoConfig).
	args := append(cfg.filterOverrides(), "diff", "--shortstat", "--no-ext-diff", "--no-textconv",
		"--no-renames", "HEAD", "--")
	stat, err := w.runner.Git(ctx, dir, args...)
	if err != nil {
		return res, err
	}
	if snap := parseShortstat(string(stat)); st.worktree == nil || *st.worktree != snap {
		res.worktree = &snap
	}
	return res, nil
}

// newCommits lists HEAD's recent commits authored by email and sorts the
// ones this process hasn't handled into those to record and those to only
// remember.
//
// Which commits count:
//   - author time within lookback. --since filters on committer time, which
//     a rebase, cherry-pick or amend refreshes; the author time check keeps
//     old work brought back by a rebase from being recorded as new;
//   - not a rewrite of a commit already recorded: amend, rebase and
//     cherry-pick keep the author time but change the hash. A new hash whose
//     author time equals a recorded commit that is no longer on HEAD is that
//     commit rewritten, and is only remembered. Two real commits made in the
//     same second both stay on HEAD, so both are recorded;
//   - not a shallow-clone boundary: git shows it without parents and diffs
//     it against the empty tree, so its stats would be the whole repository.
//     It always came from a fetch, never from work in this clone.
//
// The rewrite check can't tell a rewrite from a new commit whose author time
// is the same second as a rewritten one; that new commit is then missed.
func (w *Watcher) newCommits(ctx context.Context, dir, email string, st *repoState, now time.Time, res *scanResult) error {
	cutoff := now.Add(-lookback)
	out, err := w.runner.Git(ctx, dir, "log", "HEAD", logFormat, "--shortstat",
		"--no-ext-diff", "--no-textconv", "--no-renames", "--no-show-signature",
		"--since="+strconv.FormatInt(cutoff.Unix(), 10),
		"--max-count="+strconv.Itoa(maxCommitsPerPoll), "--")
	if err != nil {
		return err
	}
	all, err := parseLog(out)
	if err != nil {
		return err
	}
	onHead := make(map[string]bool, len(all))
	for _, c := range all {
		onHead[c.Hash] = true
	}
	taken := map[string]bool{}
	var shallow map[string]bool // loaded on first need
	// git log lists newest first; walk oldest first so events are in order.
	for _, c := range slices.Backward(all) {
		if _, done := st.seen[c.Hash]; done || c.AuthorTime.Before(cutoff) || !sameEmail(c.AuthorEmail, email) {
			continue
		}
		if old, ok := st.rewriteOf(c, onHead, taken); ok {
			taken[old] = true
			res.skip = append(res.skip, c)
			res.replaced = append(res.replaced, old)
			continue
		}
		if c.Root {
			if shallow == nil {
				if shallow, err = w.shallowCommits(ctx, dir); err != nil {
					return err
				}
			}
			if shallow[c.Hash] {
				res.skip = append(res.skip, c)
				continue
			}
		}
		res.commits = append(res.commits, c)
	}
	return nil
}

// repoConfigOut is what the sensor needs from a repo's effective config.
type repoConfigOut struct {
	email   string
	filters []string // names of configured filter drivers
}

// repoConfig reads user.email and the names of configured filter drivers in
// one git call. Values other than user.email are discarded unread.
//
// Filter drivers (filter.<name>.clean/.process) are programs git runs on
// file contents. A repo's .git/config can name any program, and a project
// folder copied with its .git (zip, USB stick, shared drive) brings its
// config along, so the sensor turns every configured filter off for its own
// git diff rather than run it every minute in the background.
func (w *Watcher) repoConfig(ctx context.Context, dir string) (repoConfigOut, error) {
	out, err := w.runner.Git(ctx, dir, "config", "-z", "--get-regexp", `^(user\.email$|filter\.)`)
	if exitCode(err) == 1 && ctx.Err() == nil {
		return repoConfigOut{}, nil // none of these keys are set
	}
	if err != nil {
		return repoConfigOut{}, err
	}
	return parseConfig(out)
}

// filterOverrides returns -c options that disable every listed filter
// driver: empty clean, smudge and process commands are no-ops to git, and
// required=false stops a missing filter from failing the diff.
func (c repoConfigOut) filterOverrides() []string {
	var args []string
	for _, name := range c.filters {
		for _, kv := range []string{".clean=", ".smudge=", ".process=", ".required=false"} {
			args = append(args, "-c", "filter."+name+kv)
		}
	}
	return args
}

// shallowCommits returns the shallow-clone boundary commits, or an empty set
// for a full clone. The list lives in .git/shallow (git metadata, one hash
// per line; never a work-tree file).
func (w *Watcher) shallowCommits(ctx context.Context, dir string) (map[string]bool, error) {
	out, err := w.runner.Git(ctx, dir, "rev-parse", "--git-path", "shallow")
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	set := map[string]bool{}
	f, err := os.Open(path) //nolint:gosec // path comes from git rev-parse for this repo
	if errors.Is(err, fs.ErrNotExist) {
		return set, nil // not shallow
	}
	if err != nil {
		return nil, fmt.Errorf("reading shallow list: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only file; close errors don't matter
	sc := bufio.NewScanner(io.LimitReader(f, maxOutput))
	for sc.Scan() {
		if h := strings.TrimSpace(sc.Text()); isHash(h) {
			set[h] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading shallow list: %w", err)
	}
	return set, nil
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
