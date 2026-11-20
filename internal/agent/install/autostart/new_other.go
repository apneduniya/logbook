//go:build !linux && !darwin && !windows

package autostart

import "errors"

// ErrUnsupported means this OS has no autostart support.
var ErrUnsupported = errors.New("autostart is not supported on this OS")

// Unsupported is the manager on OSes Logbook does not ship for.
type Unsupported struct{}

// New returns the autostart manager for this OS.
func New(Options) Unsupported { return Unsupported{} }

// Path implements the manager interface.
func (Unsupported) Path() string { return "" }

// Install implements the manager interface.
func (Unsupported) Install(string) error { return ErrUnsupported }

// Uninstall implements the manager interface.
func (Unsupported) Uninstall() error { return nil }

// Installed implements the manager interface.
func (Unsupported) Installed() (bool, error) { return false, nil }

// Start implements the manager interface.
func (Unsupported) Start() error { return ErrUnsupported }
