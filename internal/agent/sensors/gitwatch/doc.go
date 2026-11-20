// Package gitwatch is the daemon's git sensor (ADR 0009). Every 60 seconds it
// asks a RepoSource for the repositories with recent activity and, for each,
// runs the system git (never a library, never with a shell) to record:
//
//   - commits on HEAD authored in the last hour by the repository's
//     configured user.email: hash, author time, files changed, insertions
//     and deletions (git log --shortstat). Commits by anyone else, rewrites
//     (amend, rebase, cherry-pick) of commits already recorded, old work
//     brought back by a rebase, and shallow-clone boundary commits are not
//     recorded;
//   - the size of the uncommitted diff against HEAD (git diff --shortstat
//     HEAD), recorded whenever it changes.
//
// It reads only commit metadata and diff counts, never file contents, commit
// messages, author names or paths. Each git process gets a 5 second timeout,
// a hardened environment (no optional locks, no fsmonitor, no external diff,
// textconv or filter drivers, no pager, no prompts, C locale) and bounded
// output.
//
// State (commits handled, last worktree snapshot) is in memory only, and is
// updated for an event only after the sink accepted it. On start, and the
// first time a repo is seen, commits from the last hour are re-emitted, so
// the daemon must dedup commit events by (project, hash).
package gitwatch
