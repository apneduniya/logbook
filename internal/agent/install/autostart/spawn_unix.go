//go:build !windows

package autostart

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
)

// DetachedSpawner starts the daemon in its own session, so closing the
// installer's terminal does not stop it.
type DetachedSpawner struct{}

// Spawn implements Spawner.
func (DetachedSpawner) Spawn(binPath string, args ...string) error {
	cmd := exec.Command(binPath, args...) //nolint:gosec,noctx // binPath is our own installed binary; the daemon must outlive any context
	cmd.Dir = filepath.Dir(binPath)       // don't hold the installer's directory open
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", binPath, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing daemon process: %w", err)
	}
	return nil
}

func hideWindow(*exec.Cmd) {}
