//go:build linux

package osprobe

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestLinuxProbeSmoke runs the real probe against whatever this machine has
// (often a headless CI box with no session bus or X server). It checks that
// every call returns promptly and gives sane values or errors, never panics.
func TestLinuxProbeSmoke(t *testing.T) {
	p := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer func() { _ = p.Close() }()
	start := time.Now()
	if d, err := p.IdleTime(t.Context()); err == nil && d < 0 {
		t.Fatalf("negative idle %v", d)
	}
	if app, _ := p.FocusedApp(t.Context()); len(app) > maxAppNameLen {
		t.Fatalf("app name too long: %d", len(app))
	}
	_, _ = p.ScreenLocked(t.Context()) // either answer is fine here
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("probe calls took %v", el)
	}
	if got := p.Sources(); got["idle"] == "" || got["focus"] == "" {
		t.Fatalf("Sources() = %v", got)
	}
}

func TestProcessName(t *testing.T) {
	name, err := processName(os.Getpid())
	if err != nil || name == "" || len(name) > 15 {
		t.Fatalf("processName(self) = %q, %v", name, err)
	}
	if name, err := processName(1 << 30); name != "" || err != nil {
		t.Fatalf("processName(missing) = %q, %v", name, err)
	}
}
