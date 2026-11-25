package struggle

import (
	"cmp"
	"slices"
	"strings"

	"github.com/apneduniya/logbook/pkg/projectname"
)

// repoIndex maps paths to projects by longest matching path prefix.
type repoIndex []Repo

// newRepoIndex normalises repos (paths, and project names as the server does)
// and orders them so the first match is the longest (ties: project name, so
// the result never depends on input order). Entries with an empty path or
// project are ignored.
func newRepoIndex(repos []Repo) repoIndex {
	idx := make(repoIndex, 0, len(repos))
	for _, r := range repos {
		p, project := normPath(r.Path), projectname.Normalize(r.Project)
		if p == "" || project == "" {
			continue
		}
		idx = append(idx, Repo{Path: p, Project: project})
	}
	slices.SortFunc(idx, func(a, b Repo) int {
		return cmp.Or(
			cmp.Compare(len(b.Path), len(a.Path)),
			strings.Compare(a.Path, b.Path),
			strings.Compare(a.Project, b.Project),
		)
	})
	return idx
}

// project returns the project of the longest repo path containing path, or "".
// A repo matches only on a path boundary: /a/b contains /a/b/c, not /a/bc.
func (idx repoIndex) project(path string) string {
	p := normPath(path)
	if p == "" {
		return ""
	}
	for _, r := range idx {
		if p == r.Path || r.Path == "/" || strings.HasPrefix(p, r.Path+"/") {
			return r.Project
		}
	}
	return ""
}

// RepoMap resolves paths to projects exactly as the engine does for
// heartbeats (by file path) and AI events (by working directory), for
// callers that must attribute logged events the way a run did, such as a
// submission's review packet (ADR 0020).
type RepoMap struct{ idx repoIndex }

// NewRepoMap builds the map from a repo mapping, as Input.Repos holds it.
func NewRepoMap(repos []Repo) RepoMap { return RepoMap{idx: newRepoIndex(repos)} }

// Project returns the project of the longest repo path containing path,
// else fallback normalised as the server stores project names: the
// engine's rule for Heartbeat.Entity with Heartbeat.Project, and for
// AIEvent.Cwd with AIEvent.Project.
func (m RepoMap) Project(path, fallback string) string {
	if p := m.idx.project(path); p != "" {
		return p
	}
	return projectname.Normalize(fallback)
}

// normPath makes paths from any OS comparable: backslashes become slashes,
// trailing slashes go, and Windows drive paths (C:/...) are lower-cased
// because NTFS paths are case-insensitive.
func normPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	if len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0]) {
		p = strings.ToLower(p)
	}
	return p
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
