//go:build windows

package osprobe

import (
	"log/slog"
	"testing"
	"time"
)

// TestWindowsProbeSmoke runs the real Windows probe on the CI runner (which
// may have no interactive desktop). It pins the Win32 signatures: every call
// returns promptly with sane values or an error, and never panics.
func TestWindowsProbeSmoke(t *testing.T) {
	p := New(slog.New(slog.DiscardHandler))
	defer func() { _ = p.Close() }()
	start := time.Now()
	for range 3 { // repeated calls reuse the loaded procedures
		if d, err := p.IdleTime(t.Context()); err == nil && d < 0 {
			t.Fatalf("negative idle %v", d)
		}
		if app, _ := p.FocusedApp(t.Context()); len(app) > maxAppNameLen {
			t.Fatalf("app name too long: %d", len(app))
		}
		_, _ = p.ScreenLocked(t.Context()) // either answer is fine here
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("probe calls took %v", el)
	}
	if got := p.Sources(); got["idle"] == "" || got["focus"] == "" || got["lock"] == "" {
		t.Fatalf("Sources() = %v", got)
	}
}
