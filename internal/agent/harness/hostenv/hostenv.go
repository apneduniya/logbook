package hostenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/apneduniya/logbook/internal/agent/config"
)

// Env is the host as the installers see it.
type Env struct {
	// Home is the user's home directory ("" if it couldn't be found).
	Home string
	// GOOS is the target OS ("windows", "darwin", "linux", ...).
	GOOS string
	// Getenv reads an environment variable.
	Getenv func(string) string
	// LookPath finds an executable on PATH.
	LookPath func(string) (string, error)
	// BackupDir returns the directory for config backups, creating it.
	BackupDir func() (string, error)
	// Agent returns the local agent's address and token from the agent
	// config, for installers that point a harness at the daemon (Claude
	// Code's telemetry exporter).
	Agent func() (Agent, error)
}

// Agent is what an installer needs to know about the local daemon.
type Agent struct {
	// LocalAddr is the daemon's loopback address (config.Config.LocalAddr).
	LocalAddr string
	// LocalToken is the agent's local secret (config.Config.LocalToken).
	LocalToken string
}

// TestAgent is the agent ForTest reports.
var TestAgent = Agent{LocalAddr: "127.0.0.1:47231", LocalToken: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}

// ErrNoHome means the home directory is unknown, so no harness config can be
// found.
var ErrNoHome = errors.New("home directory not found")

// FromOS returns the real host's Env. Backups go to <data dir>/backups.
func FromOS() Env {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return Env{
		Home:     home,
		GOOS:     runtime.GOOS,
		Getenv:   os.Getenv,
		LookPath: exec.LookPath,
		BackupDir: func() (string, error) {
			dir, err := config.Dir()
			if err != nil {
				return "", err
			}
			return ensureDir(filepath.Join(dir, "backups"))
		},
		Agent: func() (Agent, error) {
			dir, err := config.Dir()
			if err != nil {
				return Agent{}, err
			}
			cfg, err := config.Load(dir)
			if err != nil {
				return Agent{}, fmt.Errorf("reading agent config: %w", err)
			}
			return Agent{LocalAddr: cfg.LocalAddr, LocalToken: cfg.LocalToken}, nil
		},
	}
}

// ForTest returns an Env rooted at home, with an empty environment, nothing
// on PATH, backups under backupDir and TestAgent as the agent.
func ForTest(home, goos, backupDir string) Env {
	return Env{
		Home:      home,
		GOOS:      goos,
		Getenv:    func(string) string { return "" },
		LookPath:  func(string) (string, error) { return "", exec.ErrNotFound },
		BackupDir: func() (string, error) { return ensureDir(backupDir) },
		Agent:     func() (Agent, error) { return TestAgent, nil },
	}
}

func ensureDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // dir is inside the agent's own data dir
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	return dir, nil
}

// HomePath joins elem onto the home directory.
func (e Env) HomePath(elem ...string) (string, error) {
	if e.Home == "" {
		return "", ErrNoHome
	}
	return filepath.Join(append([]string{e.Home}, elem...)...), nil
}

// DirFromEnv returns the value of the environment variable name if it is set,
// else the home-relative path elem.
func (e Env) DirFromEnv(name string, elem ...string) (string, error) {
	if v := e.Getenv(name); v != "" {
		return v, nil
	}
	return e.HomePath(elem...)
}

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// OnPath reports whether any of names is an executable on PATH.
func (e Env) OnPath(names ...string) bool {
	for _, n := range names {
		if _, err := e.LookPath(n); err == nil {
			return true
		}
	}
	return false
}
