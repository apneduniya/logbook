//go:build !windows

package autostart

// Background is a no-op off Windows: launchd and systemd start the daemon
// without a terminal, and the XDG entry sets Terminal=false.
func Background(string, []string) (bool, error) { return false, nil }
