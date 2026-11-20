//go:build darwin

package osprobe

import (
	"log/slog"
	"testing"
	"time"
)

// TestDarwinProbeSmoke runs the real macOS probe on the CI runner (which may
// have no Aqua session). It pins the purego signatures: frameworks load with
// CGO_ENABLED=0, every call returns promptly with sane values or an error,
// and nothing panics or crashes in the Objective-C runtime.
func TestDarwinProbeSmoke(t *testing.T) {
	p := New(slog.New(slog.DiscardHandler))
	defer func() { _ = p.Close() }()
	if p.loadErr != nil {
		t.Fatalf("loading frameworks through purego: %v", p.loadErr)
	}
	start := time.Now()
	for range 3 { // repeated calls reuse the loaded symbols and autorelease pools
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
