//go:build windows

package gitwatch

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideWindow stops git.exe from flashing a console window every minute when
// the daemon runs without a console (started from the Run registry key).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
