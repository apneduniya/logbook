//go:build !windows && !darwin && !linux

package osprobe

import (
	"context"
	"log/slog"
	"time"
)

var _ prober = (*Probe)(nil)

// Probe is the fallback for operating systems without a backend: every
// reading is unknown.
type Probe struct{}

// New returns a probe that reports nothing.
func New(*slog.Logger) *Probe { return &Probe{} }

// IdleTime is always unavailable.
func (*Probe) IdleTime(context.Context) (time.Duration, error) { return 0, ErrUnavailable }

// FocusedApp is always unknown.
func (*Probe) FocusedApp(context.Context) (string, error) { return "", nil }

// ScreenLocked is always unavailable.
func (*Probe) ScreenLocked(context.Context) (bool, error) { return false, ErrUnavailable }

// Sources describes the backends in use.
func (*Probe) Sources() map[string]string {
	return map[string]string{"idle": "unavailable", "focus": "unavailable", "lock": "unavailable"}
}

// Close does nothing.
func (*Probe) Close() error { return nil }
