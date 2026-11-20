//go:build windows

package autostart

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// DetachedSpawner starts the daemon in a new process group with a hidden
// console (CREATE_NO_WINDOW). A hidden console rather than DETACHED_PROCESS
// means console programs the daemon runs (git) inherit it instead of each
// opening a visible window.
type DetachedSpawner struct{}

// Spawn implements Spawner.
func (DetachedSpawner) Spawn(binPath string, args ...string) error {
	cmd := exec.Command(binPath, args...) //nolint:gosec,noctx // binPath is our own installed binary; the daemon must outlive any context
	cmd.Dir = filepath.Dir(binPath)       // don't hold the installer's directory open
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", binPath, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing daemon process: %w", err)
	}
	return nil
}

// hideWindow keeps taskkill and friends from flashing a console.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
