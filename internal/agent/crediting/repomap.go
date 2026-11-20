package crediting

import (
	"cmp"
	"slices"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// ReposAt returns the repo mapping minute m is computed with: each path's
// latest journal row (rows in the order they were added) that was recorded
// before m became final, sorted by path. A mapping that changes later never
// reaches an already-final minute, and any replay with the same rows gives
// the same mapping. It is pure.
func ReposAt(rows []localdb.RepoMapping, m time.Time) []struggle.Repo {
	cutoff := m.Add(FinalAfter)
	latest := map[string]string{}
	for _, r := range rows {
		if r.Since.Before(cutoff) {
			latest[r.Path] = r.Project
		}
	}
	out := make([]struggle.Repo, 0, len(latest))
	for p, project := range latest {
		out = append(out, struggle.Repo{Path: p, Project: project})
	}
	slices.SortFunc(out, func(a, b struggle.Repo) int { return cmp.Compare(a.Path, b.Path) })
	return out
}

// repoGroup is a run of consecutive minutes [from, to) that share one repo
// mapping.
type repoGroup struct {
	from, to time.Time
	repos    []struggle.Repo
}

// repoGroups splits [from, to) into runs of minutes with the same mapping
// (ReposAt). The open minutes all use the current mapping, so a window has
// one group unless the mapping changed in the last half hour.
func repoGroups(rows []localdb.RepoMapping, from, to time.Time) []repoGroup {
	var out []repoGroup
	for m := from; m.Before(to); m = m.Add(time.Minute) {
		repos := ReposAt(rows, m)
		if n := len(out); n > 0 && slices.Equal(out[n-1].repos, repos) {
			out[n-1].to = m.Add(time.Minute)
			continue
		}
		out = append(out, repoGroup{from: m, to: m.Add(time.Minute), repos: repos})
	}
	return out
}
