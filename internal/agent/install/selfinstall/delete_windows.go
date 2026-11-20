//go:build windows

package selfinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// deleteLater deletes path, or, when Windows refuses because the file is
// still running (usually this very process, during uninstall), starts a
// hidden cmd.exe that retries for about 30 seconds after we exit and then
// removes the directory if it is empty. No admin rights are needed, unlike
// MoveFileEx(MOVEFILE_DELAY_UNTIL_REBOOT).
func deleteLater(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	cmdLine := deleteCommand(path, filepath.Dir(path))
	cmd := exec.Command(os.Getenv("ComSpec")) //nolint:gosec,noctx // cmd.exe from the system; it must outlive this process
	if cmd.Path == "" {
		cmd = exec.Command("cmd.exe") //nolint:noctx // as above
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       cmdLine,
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("scheduling removal of %s: %w", path, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing helper process: %w", err)
	}
	return nil
}
