package osprobe

import (
	"context"
	"errors"
	"time"
)

// ErrUnavailable means the OS gives no reading for this signal right now
// (for example idle time on a Wayland desktop without a supported idle API).
var ErrUnavailable = errors.New("not available on this system")

// maxAppNameLen bounds the returned application name; wire limits it to 256.
const maxAppNameLen = 256

// prober is the method set every per-OS Probe implements. The assertion in
// each OS file keeps them identical, and cross-compiling checks it.
type prober interface {
	// IdleTime returns the time since the last user input on this session.
	IdleTime(ctx context.Context) (time.Duration, error)
	// FocusedApp returns the focused application's process name (macOS:
	// bundle id, or app name when it has none), or "" when unknown.
	FocusedApp(ctx context.Context) (string, error)
	// ScreenLocked reports whether the session's screen is locked.
	ScreenLocked(ctx context.Context) (bool, error)
	// Sources names the backends in use, for `logbook doctor`.
	Sources() map[string]string
	// Close releases OS connections.
	Close() error
}

// truncateName cuts s to maxAppNameLen bytes without splitting a UTF-8 rune.
func truncateName(s string) string {
	if len(s) <= maxAppNameLen {
		return s
	}
	cut := maxAppNameLen
	for cut > 0 && s[cut]&0xC0 == 0x80 { // continuation byte
		cut--
	}
	return s[:cut]
}
