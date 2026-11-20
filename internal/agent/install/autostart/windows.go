package autostart

import (
	"fmt"
	"strconv"
	"strings"
)

// RunValueName is the value written under the HKCU Run key.
const RunValueName = "Logbook"

// RunKey is HKCU\Software\Microsoft\Windows\CurrentVersion\Run.
type RunKey interface {
	Get(name string) (value string, ok bool, err error)
	Set(name, value string) error
	Delete(name string) error // a missing value is not an error
}

// Windows manages the HKCU Run value. Windows starts it at login; Start
// starts it now, hidden and detached.
type Windows struct {
	key     RunKey
	run     Runner
	spawn   Spawner
	selfPID int
	bin     string
}

// NewWindows returns a Windows manager. selfPID is this process, which
// stopping other daemons must never kill.
func NewWindows(key RunKey, r Runner, s Spawner, selfPID int) *Windows {
	return &Windows{key: key, run: r, spawn: s, selfPID: selfPID}
}

// Path describes the entry, for the install summary.
func (w *Windows) Path() string {
	return `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\` + RunValueName
}

// Install sets the Run value to start binPath at login.
func (w *Windows) Install(binPath string) error {
	w.bin = binPath
	want := WindowsRunValue(binPath)
	have, ok, err := w.key.Get(RunValueName)
	if err != nil {
		return fmt.Errorf("reading Run key: %w", err)
	}
	if ok && have == want {
		return nil
	}
	if err := w.key.Set(RunValueName, want); err != nil {
		return fmt.Errorf("writing Run key: %w", err)
	}
	return nil
}

// Start stops any running daemon (it may be the old binary, holding the
// port) and starts the installed one without a console window.
func (w *Windows) Start() error {
	if w.bin == "" {
		return errNotInstalled
	}
	w.stop()
	return w.spawn.Spawn(w.bin, runArg)
}

// Uninstall removes the Run value and stops running daemons, so the binary
// can be deleted.
func (w *Windows) Uninstall() error {
	if err := w.key.Delete(RunValueName); err != nil {
		return fmt.Errorf("deleting Run value: %w", err)
	}
	w.stop()
	return nil
}

// Installed reports whether the Run value exists.
func (w *Windows) Installed() (bool, error) {
	_, ok, err := w.key.Get(RunValueName)
	if err != nil {
		return false, fmt.Errorf("reading Run key: %w", err)
	}
	return ok, nil
}

// stop ends every other logbook.exe of this user. taskkill fails when none
// match, and a process that already exited is not a problem, so its error
// is deliberately ignored; Spawn reports real trouble.
func (w *Windows) stop() {
	image := "logbook.exe"
	if w.bin != "" {
		// Split on both separators so the logic is the same when tested
		// off Windows.
		image = w.bin[strings.LastIndexAny(w.bin, `\/`)+1:]
	}
	_ = run(w.run, "taskkill", "/F", "/IM", image, "/FI", "PID ne "+strconv.Itoa(w.selfPID))
}
