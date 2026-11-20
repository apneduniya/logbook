package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/wire"
)

func TestCodingTime(t *testing.T) {
	base := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	at := func(sec ...int) []time.Time {
		out := make([]time.Time, len(sec))
		for i, s := range sec {
			out[i] = base.Add(time.Duration(s) * time.Second)
		}
		return out
	}
	tests := []struct {
		name  string
		times []time.Time
		want  time.Duration
	}{
		{"none", nil, 0},
		{"one", at(0), 0},
		{"short gaps", at(0, 30, 90), 90 * time.Second},
		{"exactly two minutes", at(0, 120), 2 * time.Minute},
		{"long gap credits nothing", at(0, 60, 400, 430), 90 * time.Second},
	}
	for _, tt := range tests {
		if got := CodingTime(tt.times); got != tt.want {
			t.Errorf("%s: CodingTime = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDurationText(t *testing.T) {
	tests := map[int64]string{0: "0 secs", 1: "1 sec", 7: "7 secs", 60: "1 min", 2940: "49 mins",
		3600: "1 hr", 3660: "1 hr 1 min", 8220: "2 hrs 17 mins"}
	for in, want := range tests {
		if got := DurationText(in); got != want {
			t.Errorf("DurationText(%d) = %q, want %q", in, got, want)
		}
	}
}

type fakeHarness struct {
	name string
	ok   bool
	err  error
}

func (f fakeHarness) Name() string             { return f.name }
func (f fakeHarness) Installed() (bool, error) { return f.ok, f.err }

func TestCollect(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := localdb.Open(ctx, config.DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now()
	start := now.Add(-90 * time.Second)
	if start.Before(StartOfDay(now)) {
		t.Skip("too close to midnight for a same-day heartbeat pair")
	}
	unix := func(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }
	hbs := []wire.Heartbeat{
		{Entity: "/a.go", Time: unix(start)},
		{Entity: "/a.go", Time: unix(start.Add(time.Minute))},
	}
	if err := db.Enqueue(ctx, hbs, true); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.SpoolPath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.SpoolPath(dir), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ServerURL: "https://lb.example", APIKey: "secret-key",
		Hackatime: config.Hackatime{URL: "https://hackatime.hackclub.com/api/hackatime/v1", APIKey: "hk", Mirror: true}}
	s, err := Collect(ctx, Input{
		Store: db, Config: cfg, DataDir: dir, Version: "v1", Now: now,
		Harnesses: []Harness{fakeHarness{"claude-code", true, nil}, fakeHarness{"codex", false, errors.New("boom")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.SignedIn || s.MirrorHost != "hackatime.hackclub.com" || s.Logbook.Queue.Pending != 2 || s.Hackatime.Queue.Pending != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.Today.Heartbeats != 2 || s.Today.CodingSeconds != 60 || s.Today.CodingText != "1 min" {
		t.Fatalf("today = %+v", s.Today)
	}
	if len(s.Hooks) != 2 || s.Hooks[0].State != "installed" || s.Hooks[1].State != "error: boom" {
		t.Fatalf("hooks = %+v", s.Hooks)
	}
	if s.SpoolBytes != 3 {
		t.Fatalf("spool bytes = %d", s.SpoolBytes)
	}
	s2, err := Collect(ctx, Input{Store: db, Config: config.Config{}, Now: now})
	if err != nil || s2.Hooks != nil || s2.SignedIn || s2.MirrorHost != "" {
		t.Fatalf("unknown hooks snapshot = %+v, %v", s2, err)
	}
}
