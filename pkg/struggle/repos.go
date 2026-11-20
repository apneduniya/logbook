package struggle

import (
	"cmp"
	"slices"
	"strings"
)

// repoIndex maps paths to projects by longest matching path prefix.
type repoIndex []Repo

// newRepoIndex normalises repos and orders them so the first match is the
// longest (ties: project name, so the result never depends on input order).
// Entries with an empty path or project are ignored.
func newRepoIndex(repos []Repo) repoIndex {
	idx := make(repoIndex, 0, len(repos))
	for _, r := range repos {
		p := normPath(r.Path)
		if p == "" || r.Project == "" {
			continue
		}
		idx = append(idx, Repo{Path: p, Project: r.Project})
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
