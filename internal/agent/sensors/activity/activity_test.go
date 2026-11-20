package activity

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/sensors/focusclass"
	"github.com/apneduniya/logbook/pkg/wire"
)

// reading is what the fake probe returns for one sample.
type reading struct {
	idle    time.Duration
	idleErr error
	app     string
	locked  bool
	lockErr error
}

type fakeProbe struct {
	readings   []reading
	i          int
	focusCalls int
}

func (f *fakeProbe) cur() reading { return f.readings[min(f.i, len(f.readings)-1)] }

func (f *fakeProbe) IdleTime(context.Context) (time.Duration, error) {
	r := f.cur()
	return r.idle, r.idleErr
}

func (f *fakeProbe) FocusedApp(context.Context) (string, error) {
	f.focusCalls++
	return f.cur().app, nil
}

func (f *fakeProbe) ScreenLocked(context.Context) (bool, error) {
	r := f.cur()
	return r.locked, r.lockErr
}

type fakeSink struct {
	samples []wire.ActivitySample
	err     error
}

func (f *fakeSink) RecordActivity(_ context.Context, s wire.ActivitySample) error {
	f.samples = append(f.samples, s)
	return f.err
}

func (f *fakeSink) RecordGit(context.Context, wire.GitEvent) error { return nil }

// run drives the sensor through every reading and returns the waits it chose.
func run(t *testing.T, probe *fakeProbe, sink *fakeSink) []time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waits []time.Duration
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.FixedZone("IST", 19800))
	s := New(probe,
		WithClock(func() time.Time { return now }),
		WithClassifier(focusclass.New("linux")),
		WithSleep(func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			now = now.Add(d)
			probe.i++
			if probe.i >= len(probe.readings) {
				cancel()
				return ctx.Err()
			}
			return nil
		}),
	)
	if err := s.Run(ctx, sink); err != nil {
		t.Fatalf("Run() = %v, want nil on cancel", err)
	}
	return waits
}

func TestCadence(t *testing.T) {
	errNoIdle := errors.New("no idle source")
	tests := []struct {
		name      string
		readings  []reading
		wantWaits []time.Duration
		wantN     int // samples recorded
	}{
		{"active user samples every 5s",
			[]reading{{idle: time.Second, app: "code"}, {idle: 2 * time.Second, app: "code"}},
			[]time.Duration{5 * time.Second, 5 * time.Second}, 2},
		{"idle past 10 minutes slows to 30s and back",
			[]reading{{idle: 9 * time.Minute}, {idle: 10 * time.Minute}, {idle: 11 * time.Minute}, {idle: 0}},
			[]time.Duration{5 * time.Second, 30 * time.Second, 30 * time.Second, 5 * time.Second}, 4},
		{"locked screen slows to 30s",
			[]reading{{idle: time.Second, locked: true}, {idle: time.Second}},
			[]time.Duration{30 * time.Second, 5 * time.Second}, 2},
		{"unknown idle emits nothing and polls slowly",
			[]reading{{idleErr: errNoIdle, app: "code"}, {idleErr: errNoIdle}},
			[]time.Duration{30 * time.Second, 30 * time.Second}, 0},
		{"lock check failure is treated as unlocked",
			[]reading{{idle: time.Second, lockErr: errors.New("no logind")}},
			[]time.Duration{5 * time.Second}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &fakeSink{}
			waits := run(t, &fakeProbe{readings: tt.readings}, sink)
			if !slices.Equal(waits, tt.wantWaits) {
				t.Fatalf("waits = %v, want %v", waits, tt.wantWaits)
			}
			if len(sink.samples) != tt.wantN {
				t.Fatalf("recorded %d samples, want %d", len(sink.samples), tt.wantN)
			}
		})
	}
}

func TestSampleContents(t *testing.T) {
	probe := &fakeProbe{readings: []reading{
		{idle: 1234 * time.Millisecond, app: "gnome-terminal-"},
		{idle: 0, app: ""},
		{idle: time.Minute, app: "code", locked: true},
		{idle: -time.Second, app: "nautilus"},
	}}
	sink := &fakeSink{}
	run(t, probe, sink)
	want := []struct {
		idle float64
		app  string
		kind string
	}{
		{1.2, "gnome-terminal-", "terminal"},
		{0, "", "unknown"},
		{60, "", "unknown"}, // locked: focus not read
		{0, "nautilus", "other"},
	}
	if len(sink.samples) != len(want) {
		t.Fatalf("got %d samples", len(sink.samples))
	}
	for i, w := range want {
		s := sink.samples[i]
		if s.IdleSeconds != w.idle || s.FocusApp != w.app || s.FocusKind != w.kind {
			t.Errorf("sample %d = %+v, want idle=%v app=%q kind=%q", i, s, w.idle, w.app, w.kind)
		}
		if s.Time.Location() != time.UTC {
			t.Errorf("sample %d time not UTC: %v", i, s.Time)
		}
		if err := s.Validate(); err != nil {
			t.Errorf("sample %d invalid: %v", i, err)
		}
	}
	if probe.focusCalls != 3 {
		t.Errorf("FocusedApp called %d times, want 3 (skipped while locked)", probe.focusCalls)
	}
}

func TestSinkErrorsDoNotStopSensor(t *testing.T) {
	sink := &fakeSink{err: errors.New("db busy")}
	waits := run(t, &fakeProbe{readings: []reading{{idle: 0}, {idle: 0}, {idle: 0}}}, sink)
	if len(waits) != 3 || len(sink.samples) != 3 {
		t.Fatalf("waits=%d samples=%d, want 3 each", len(waits), len(sink.samples))
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := New(&fakeProbe{readings: []reading{{idle: 0}}}, WithIntervals(time.Hour, time.Hour))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, &fakeSink{}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// The sensor must satisfy the daemon's Sensor contract by method set.
var _ interface {
	Name() string
	Run(ctx context.Context, sink Sink) error
} = (*Sensor)(nil)
