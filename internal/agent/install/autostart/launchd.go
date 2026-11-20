package autostart

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/apneduniya/logbook/internal/agent/install/atomicfile"
)

// Launchd manages the macOS LaunchAgent.
type Launchd struct {
	plistPath string
	logPath   string
	domain    string // gui/<uid>
	run       Runner
}

// NewLaunchd returns a Launchd for the user with home directory home and
// numeric id uid. The job's stdout and stderr go to dataDir/launchd.log.
func NewLaunchd(home, dataDir string, uid int, r Runner) *Launchd {
	return &Launchd{
		plistPath: filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
		logPath:   filepath.Join(dataDir, "launchd.log"),
		domain:    "gui/" + strconv.Itoa(uid),
		run:       r,
	}
}

// Path returns the plist path.
func (l *Launchd) Path() string { return l.plistPath }

func (l *Launchd) service() string { return l.domain + "/" + Label }

// Install writes the plist and loads it into the user's GUI domain, which
// also starts the job (RunAtLoad). An unchanged, already loaded job is left
// alone; a changed one is unloaded first, as bootstrap refuses to load a
// label twice.
func (l *Launchd) Install(binPath string) error {
	want := LaunchdPlist(binPath, l.logPath)
	have, _, err := atomicfile.ReadOptional(l.plistPath)
	if err != nil {
		return err
	}
	loaded := l.loaded()
	if bytes.Equal(have, want) && loaded {
		return nil
	}
	if err := atomicfile.Write(l.plistPath, want, 0o644); err != nil {
		return fmt.Errorf("writing LaunchAgent: %w", err)
	}
	if loaded {
		if err := run(l.run, "launchctl", "bootout", l.service()); err != nil {
			return fmt.Errorf("unloading old LaunchAgent: %w", err)
		}
	}
	if err := run(l.run, "launchctl", "bootstrap", l.domain, l.plistPath); err != nil {
		return fmt.Errorf("loading LaunchAgent: %w", err)
	}
	return nil
}

// Start (re)starts the job so the binary just installed is the one running.
func (l *Launchd) Start() error {
	if err := run(l.run, "launchctl", "kickstart", "-k", l.service()); err != nil {
		return fmt.Errorf("starting LaunchAgent: %w", err)
	}
	return nil
}

// Uninstall stops and unloads the job and deletes the plist.
func (l *Launchd) Uninstall() error {
	if l.loaded() {
		if err := run(l.run, "launchctl", "bootout", l.service()); err != nil {
			return fmt.Errorf("unloading LaunchAgent: %w", err)
		}
	}
	if _, err := atomicfile.RemoveOptional(l.plistPath); err != nil {
		return err
	}
	return nil
}

// Installed reports whether the plist exists.
func (l *Launchd) Installed() (bool, error) {
	_, err := os.Stat(l.plistPath)
	return statExists(err)
}

// loaded reports whether launchd knows the job. "launchctl print" fails for
// an unknown service.
func (l *Launchd) loaded() bool {
	err := run(l.run, "launchctl", "print", l.service())
	return err == nil
}
