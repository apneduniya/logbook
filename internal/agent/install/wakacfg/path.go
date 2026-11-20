package wakacfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Env is the part of the process environment the locator reads. Tests pass
// a fake; the command passes OSEnv.
type Env struct {
	Getenv  func(string) string
	HomeDir func() (string, error)
	// Exists reports whether path exists. Any error other than "not
	// exist" counts as existing, as in wakatime-cli.
	Exists func(path string) bool
}

// OSEnv returns an Env backed by the real environment and filesystem.
func OSEnv() Env {
	return Env{
		Getenv:  os.Getenv,
		HomeDir: os.UserHomeDir,
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return !errors.Is(err, fs.ErrNotExist)
		},
	}
}

// Locate returns the path of the main config file wakatime-cli reads, using
// its rules (wakatime-cli pkg/ini.FilePath, USAGE.md "Config File"):
//
//  1. $WAKATIME_HOME/.wakatime.cfg when WAKATIME_HOME is non-empty;
//  2. ~/.wakatime.cfg when it exists;
//  3. $XDG_CONFIG_HOME/wakatime/wakatime.cfg when that file exists
//     (~/.config when XDG_CONFIG_HOME is unset or relative);
//  4. otherwise ~/.wakatime.cfg.
//
// Step 4 differs from wakatime-cli, which would pick the XDG path for a
// brand-new file: several editor plugins read and create ~/.wakatime.cfg
// directly, and once it exists wakatime-cli prefers it, so creating the
// legacy file is the choice every reader agrees on.
func Locate(env Env) (string, error) {
	if h := env.Getenv("WAKATIME_HOME"); h != "" {
		h, err := expandHome(h, env)
		if err == nil {
			return filepath.Join(h, ".wakatime.cfg"), nil
		}
		// wakatime-cli falls back to the user's home in this case too.
	}
	home, err := env.HomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	if home == "" {
		return "", errors.New("finding home directory: none set")
	}
	legacy := filepath.Join(home, ".wakatime.cfg")
	if env.Exists(legacy) {
		return legacy, nil
	}
	configHome := env.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configHome) {
		configHome = filepath.Join(home, ".config")
	}
	if xdg := filepath.Join(configHome, "wakatime", "wakatime.cfg"); env.Exists(xdg) {
		return xdg, nil
	}
	return legacy, nil
}

// expandHome expands a leading "~" the way go-homedir does.
func expandHome(p string, env Env) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	if len(p) > 1 && p[1] != '/' && p[1] != '\\' {
		return "", errors.New("cannot expand user-specific home dir")
	}
	home, err := env.HomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	return filepath.Join(home, p[1:]), nil
}
