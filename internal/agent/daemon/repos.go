package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
)

// Repo discovery bounds.
const (
	repoWindow    = time.Hour
	maxRecentDirs = 200
	maxRepos      = 50
	maxWalkDepth  = 64
	rootCacheTTL  = 10 * time.Minute
	maxCacheSize  = 1000
)

// DirSource lists directories recently seen in heartbeats and AI events.
// *localdb.DB satisfies it.
type DirSource interface {
	RecentDirs(ctx context.Context, since time.Time, limit int) ([]localdb.DirSeen, error)
}

// StatFunc reports whether path exists (os.Stat in production).
type StatFunc func(path string) bool

// Repos implements RepoSource from recent heartbeat entity directories and
// AI event working directories: each is walked up to the nearest directory
// containing .git (a directory, or a file for worktrees and submodules).
type Repos struct {
	dirs   DirSource
	exists StatFunc
	now    func() time.Time

	mu    sync.Mutex
	roots map[string]rootEntry // dir -> git root ("" if none)
}

type rootEntry struct {
	root string
	at   time.Time
}

// NewRepos returns a RepoSource over dirs. exists may be nil for os.Stat.
func NewRepos(dirs DirSource, exists StatFunc) *Repos {
	if exists == nil {
		exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	return &Repos{dirs: dirs, exists: exists, now: time.Now, roots: map[string]rootEntry{}}
}

// ActiveRepos returns git repositories with activity in the last hour, most
// recently active first. Project is the name the heartbeats or AI events
// used for it, or else the repository directory's name.
func (r *Repos) ActiveRepos(ctx context.Context) ([]Repo, error) {
	seen, err := r.dirs.RecentDirs(ctx, r.now().Add(-repoWindow), maxRecentDirs)
	if err != nil {
		return nil, err
	}
	var out []Repo
	index := map[string]int{}
	for _, d := range seen {
		if !filepath.IsAbs(d.Dir) {
			continue
		}
		root := r.gitRoot(filepath.Clean(d.Dir))
		if root == "" {
			continue
		}
		if i, ok := index[root]; ok {
			if out[i].Project == filepath.Base(root) && d.Project != "" {
				out[i].Project = d.Project
			}
			continue
		}
		if len(out) == maxRepos {
			continue
		}
		project := d.Project
		if project == "" {
			project = filepath.Base(root)
		}
		index[root] = len(out)
		out = append(out, Repo{Path: root, Project: project})
	}
	return out, nil
}

// gitRoot returns the nearest ancestor of dir (inclusive) containing .git,
// or "". Results are cached briefly so the 60 s git poll stays cheap.
func (r *Repos) gitRoot(dir string) string {
	now := r.now()
	r.mu.Lock()
	if e, ok := r.roots[dir]; ok && now.Sub(e.at) < rootCacheTTL {
		r.mu.Unlock()
		return e.root
	}
	r.mu.Unlock()

	root := ""
	cur := dir
	for range maxWalkDepth {
		if r.exists(filepath.Join(cur, ".git")) {
			root = cur
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	r.mu.Lock()
	if len(r.roots) >= maxCacheSize {
		r.roots = map[string]rootEntry{}
	}
	r.roots[dir] = rootEntry{root: root, at: now}
	r.mu.Unlock()
	return root
}
