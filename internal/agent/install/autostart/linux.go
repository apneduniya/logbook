package autostart

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/apneduniya/logbook/internal/agent/install/atomicfile"
)

// unitName is the systemd --user service name.
const unitName = "logbook.service"

// daemonPattern matches the daemon's command line for pkill -f: any path
// ending in "logbook", then " run". It never matches "logbook install".
const daemonPattern = `(^|/)logbook run$`

// errNotInstalled means Start ran before Install in this process and no
// entry exists to start from.
var errNotInstalled = errors.New("autostart is not installed")

// Linux manages a systemd --user unit, falling back to an XDG autostart
// entry when the user has no systemd user manager (WSL, some minimal
// distros, containers).
type Linux struct {
	unitPath    string
	desktopPath string
	run         Runner
	spawn       Spawner
	bin         string
}

// NewLinux returns a Linux manager. configHome is $XDG_CONFIG_HOME, or
// ~/.config when it is unset (os.UserConfigDir).
func NewLinux(configHome string, r Runner, s Spawner) *Linux {
	return &Linux{
		unitPath:    filepath.Join(configHome, "systemd", "user", unitName),
		desktopPath: filepath.Join(configHome, "autostart", "logbook.desktop"),
		run:         r,
		spawn:       s,
	}
}

// Path returns the entry in use: the unit when systemd is available,
// otherwise the .desktop file.
func (l *Linux) Path() string {
	if l.hasSystemd() {
		return l.unitPath
	}
	return l.desktopPath
}

// hasSystemd reports whether a systemd user manager is reachable.
func (l *Linux) hasSystemd() bool {
	err := run(l.run, "systemctl", "--user", "show-environment")
	return err == nil
}

// Install writes the unit and enables it, or writes the XDG entry. It
// removes the other kind, so switching between them never starts two
// daemons.
func (l *Linux) Install(binPath string) error {
	l.bin = binPath
	if !l.hasSystemd() {
		if err := writeIfChanged(l.desktopPath, DesktopEntry(binPath)); err != nil {
			return fmt.Errorf("writing autostart entry: %w", err)
		}
		return nil
	}
	if err := writeIfChanged(l.unitPath, SystemdUnit(binPath)); err != nil {
		return fmt.Errorf("writing systemd unit: %w", err)
	}
	if err := run(l.run, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := run(l.run, "systemctl", "--user", "enable", unitName); err != nil {
		return err
	}
	if _, err := atomicfile.RemoveOptional(l.desktopPath); err != nil {
		return err
	}
	return nil
}

// Start (re)starts the daemon so the binary just installed is running.
func (l *Linux) Start() error {
	unit, err := exists(l.unitPath)
	if err != nil {
		return err
	}
	if unit && l.hasSystemd() {
		return run(l.run, "systemctl", "--user", "restart", unitName)
	}
	if l.bin == "" {
		return errNotInstalled
	}
	if err := l.stopDetached(); err != nil {
		return err
	}
	return l.spawn.Spawn(l.bin, runArg)
}

// Uninstall stops the daemon and removes both kinds of entry.
func (l *Linux) Uninstall() error {
	unit, err := exists(l.unitPath)
	if err != nil {
		return err
	}
	if unit && l.hasSystemd() {
		// "disable --now" also stops it. A unit that was never enabled
		// fails here; removing the file below still cleans up.
		_ = run(l.run, "systemctl", "--user", "disable", "--now", unitName)
	}
	if _, err := atomicfile.RemoveOptional(l.unitPath); err != nil {
		return err
	}
	if unit && l.hasSystemd() {
		if err := run(l.run, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
	}
	desktop, err := exists(l.desktopPath)
	if err != nil {
		return err
	}
	if desktop {
		if err := l.stopDetached(); err != nil {
			return err
		}
		if _, err := atomicfile.RemoveOptional(l.desktopPath); err != nil {
			return err
		}
	}
	return nil
}

// Installed reports whether either entry exists.
func (l *Linux) Installed() (bool, error) {
	unit, err := exists(l.unitPath)
	if err != nil || unit {
		return unit, err
	}
	return exists(l.desktopPath)
}

// stopDetached ends a daemon started outside systemd. pkill exits 1 when
// nothing matched, which is fine.
func (l *Linux) stopDetached() error {
	err := run(l.run, "pkill", "-f", daemonPattern)
	if err != nil && exitCode(err) != 1 && !errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("stopping running daemon: %w", err)
	}
	return nil
}

func writeIfChanged(path string, want []byte) error {
	have, _, err := atomicfile.ReadOptional(path)
	if err != nil {
		return err
	}
	if bytes.Equal(have, want) {
		return nil
	}
	return atomicfile.Write(path, want, 0o644)
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	return statExists(err)
}

func statExists(err error) (bool, error) {
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking autostart entry: %w", err)
	}
	return true, nil
}
