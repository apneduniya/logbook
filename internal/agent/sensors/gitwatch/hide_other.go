//go:build !windows

package gitwatch

import "os/exec"

// hideWindow is a no-op outside Windows.
func hideWindow(*exec.Cmd) {}
