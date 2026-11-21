package crediting

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A wake is a gap by either clock: a suspend shows on the wall clock while
// the monotonic clock may not count it, and a long gap shows on the
// monotonic clock even if the wall clock was set back meanwhile.
func TestWokeByEitherClock(t *testing.T) {
	gap := 3 * time.Minute
	tests := []struct {
		name       string
		wall, mono time.Duration
		want       bool
	}{
		{"a minute on both", time.Minute, time.Minute, false},
		{"suspend: wall clock only", 2 * time.Hour, time.Minute, true},
		{"clock set back during a long gap: monotonic only", time.Minute, 2 * time.Hour, true},
		{"clock set back past the last run", -5 * time.Minute, time.Minute, true},
		{"just within the gap", gap, gap, false},
	}
	for _, tt := range tests {
		if got := woke(tt.wall, tt.mono, gap); got != tt.want {
			t.Errorf("%s: woke = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Run makes its first run only once the startup spool drain has finished,
// so it starts from every event the hooks spooled while the agent was down.
func TestRunWaitsForTheSpoolDrain(t *testing.T) {
	e := newEnv(t, t0)
	drained := make(chan struct{})
	e.options = func(o *Options) { o.Drained, o.Interval = drained, 5*time.Millisecond }
	e.open()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.runner.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if n := e.computeCalls(); n != 0 {
		t.Fatalf("%d runs before the drain finished", n)
	}
	close(drained)
	deadline := time.Now().Add(5 * time.Second)
	for e.computeCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if e.computeCalls() == 0 {
		t.Fatal("no run after the drain finished")
	}
}

// After a start, minutes old enough that the server would freeze them
// before the agent settles are computed but not uploaded, and nothing is
// final; once settled they are made final and uploaded. A gap in runs (a
// sleep) starts the settle period again.
func TestSettlePeriodHoldsOldMinutes(t *testing.T) {
	e := newEnv(t, t0) // 12:00:05
	e.options = func(o *Options) { o.Settle = DefaultSettle }
	e.open()
	e.credit(at("11:20"), "app", 60) // 40 minutes old: final on the server at once
	e.credit(at("11:55"), "app", 60) // still open on the server after the settle period
	e.run()
	if _, ok := e.srv.minute(e.deviceID(), at("11:20"), "app"); ok {
		t.Fatal("11:20 uploaded before the agent settled")
	}
	e.wantServerMinute(at("11:55"), "app", 60)
	s := e.status()
	if !strings.Contains(s.Minutes.Settling, "2026-11-20T12:15:05Z") || s.Minutes.Waiting != 1 {
		t.Fatalf("status while settling = %+v", s.Minutes)
	}
	// A late event changes 11:20 while settling: it is rewritten.
	e.credit(at("11:20"), "app", 30)
	for range 15 {
		e.advance(time.Minute)
		e.run()
	}
	e.wantServerMinute(at("11:20"), "app", 30)
	if s := e.status(); s.Minutes.Settling != "" || s.Minutes.Waiting != 0 {
		t.Fatalf("status once settled = %+v", s.Minutes)
	}

	// The machine sleeps for an hour: settling again.
	e.advance(time.Hour)
	e.credit(e.clock().Truncate(time.Minute).Add(-40*time.Minute), "app", 60)
	e.run()
	if s := e.status(); s.Minutes.Settling == "" {
		t.Fatal("no settle period after a gap in runs")
	}
}
