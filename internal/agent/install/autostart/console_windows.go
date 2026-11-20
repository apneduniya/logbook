//go:build windows

package autostart

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Background makes the login-started daemon windowless. `logbook run` calls
// it first: when it returns true, a hidden copy of the daemon with the same
// arguments is running and the caller must exit at once. The visible
// console flashes briefly at login and then closes; a hidden copy started
// by Start never shows one.
func Background(binPath string, args []string) (bool, error) {
	if !needsRelaunch(os.Getenv(DetachedEnv), consoleProcessCount()) {
		return false, nil
	}
	if err := (DetachedSpawner{}).Spawn(binPath, args...); err != nil {
		return false, fmt.Errorf("relaunching without a console: %w", err)
	}
	return true, nil
}

// consoleProcessCount returns how many processes share this console, or 0
// when there is none or the call fails (then nothing is relaunched).
func consoleProcessCount() int {
	// GetConsoleProcessList is not wrapped by golang.org/x/sys/windows.
	// It runs once per daemon start, so the lookup is not cached.
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	if proc.Find() != nil {
		return 0
	}
	var pids [4]uint32
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids))) //nolint:errcheck,gosec // a 0 count is the failure signal; the pointer is to a local array that outlives the call
	return int(n)
}
