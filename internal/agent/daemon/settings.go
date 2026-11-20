package daemon

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
)

// settingsCheckEvery is how often the config file's mtime is checked.
const settingsCheckEvery = 5 * time.Second

// settings serves the current config, reloading config.json when it
// changes on disk (e.g. after `logbook login`), so a new key applies
// without restarting the daemon. A config that fails to load is ignored
// and the last good one kept.
type settings struct {
	dir   string
	log   *slog.Logger
	now   func() time.Time
	every time.Duration

	mu      sync.Mutex
	cfg     config.Config
	modTime time.Time
	checked time.Time
}

func newSettings(dir string, cfg config.Config, log *slog.Logger, every time.Duration) *settings {
	if every <= 0 {
		every = settingsCheckEvery
	}
	s := &settings{dir: dir, cfg: cfg, log: log, now: time.Now, every: every}
	if fi, err := os.Stat(config.Path(dir)); err == nil {
		s.modTime = fi.ModTime()
	}
	s.checked = s.now()
	return s
}

// Get returns the current config. It is cheap enough to call per request.
func (s *settings) Get() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.checked) < s.every {
		return s.cfg
	}
	s.checked = now
	fi, err := os.Stat(config.Path(s.dir))
	if err != nil || fi.ModTime().Equal(s.modTime) {
		return s.cfg
	}
	cfg, err := config.Load(s.dir)
	if err != nil {
		s.log.Warn("config changed but could not be loaded; keeping the previous one", "err", err)
		s.modTime = fi.ModTime() // don't retry every few seconds; wait for the next change
		return s.cfg
	}
	if cfg.LocalAddr != s.cfg.LocalAddr {
		s.log.Warn("local_addr changed; restart the agent to listen on the new address")
	}
	s.log.Info("config reloaded")
	s.cfg, s.modTime = cfg, fi.ModTime()
	return s.cfg
}
