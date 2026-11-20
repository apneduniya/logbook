package osprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestIdleFromTicks(t *testing.T) {
	const wrap = uint64(1) << 32
	tests := []struct {
		name string
		now  uint64
		last uint32
		want time.Duration
	}{
		{"just now", 10_000, 10_000, 0},
		{"five seconds", 15_000, 10_000, 5 * time.Second},
		{"after 50 days, no wrap in between", 50*24*3600*1000 + 2000, uint32((50*24*3600*1000 + 1000) % wrap), time.Second},
		{"wrapped between input and now", wrap + 500, uint32(wrap - 1500), 2 * time.Second},
		{"exactly at wrap", wrap, uint32(wrap - 1), time.Millisecond},
		{"second wrap", 2*wrap + 100, uint32(wrap - 100), 200 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idleFromTicks(tt.now, tt.last); got != tt.want {
				t.Fatalf("idleFromTicks(%d, %d) = %v, want %v", tt.now, tt.last, got, tt.want)
			}
		})
	}
}

func TestChainToleratesTimeouts(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	mutter := &fakeSource{idle: time.Second}
	logind := &fakeSource{idle: 9 * time.Second}
	c := newIdleChain([]idleSource{{"mutter", mutter.read}, {"logind", logind.read}},
		func() time.Time { return now }, slog.New(slog.DiscardHandler))
	if _, err := c.Idle(t.Context()); err != nil || c.Current() != "mutter" {
		t.Fatalf("start: %v %s", err, c.Current())
	}

	// A slow reply costs that sample only; the source is kept.
	mutter.err = fmt.Errorf("d-bus call: %w", context.DeadlineExceeded)
	for i := range maxTimeouts - 1 {
		if _, err := c.Idle(t.Context()); !errors.Is(err, ErrUnavailable) || c.Current() != "mutter" {
			t.Fatalf("timeout %d: err=%v current=%s", i, err, c.Current())
		}
	}
	mutter.err = nil
	if d, err := c.Idle(t.Context()); err != nil || d != time.Second || c.Current() != "mutter" {
		t.Fatalf("after recovery: %v %v %s", d, err, c.Current())
	}
	if logind.calls != 0 {
		t.Fatalf("fell through to logind %d times", logind.calls)
	}

	// Repeated timeouts do switch.
	mutter.err = fmt.Errorf("d-bus call: %w", context.DeadlineExceeded)
	for range maxTimeouts {
		_, _ = c.Idle(t.Context())
	}
	if c.Current() != "logind" {
		t.Fatalf("after %d timeouts current = %s", maxTimeouts, c.Current())
	}

	// The caller's own cancellation says nothing about the source.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	logind.err = context.Canceled
	if _, err := c.Idle(ctx); !errors.Is(err, context.Canceled) || c.Current() != "logind" {
		t.Fatalf("cancelled: err=%v current=%s", err, c.Current())
	}
}

// fakeSource is an idle source whose result the test controls.
type fakeSource struct {
	calls int
	err   error
	idle  time.Duration
}

func (f *fakeSource) read(context.Context) (time.Duration, error) {
	f.calls++
	return f.idle, f.err
}

func TestIdleChain(t *testing.T) {
	errDown := errors.New("down")
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mutter := &fakeSource{err: errDown}
	saver := &fakeSource{err: errDown}
	logind := &fakeSource{idle: 3 * time.Second}
	c := newIdleChain([]idleSource{
		{"mutter", mutter.read}, {"saver", saver.read}, {"logind", logind.read},
	}, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// First call scans in order and settles on the first source that works.
	if d, err := c.Idle(t.Context()); err != nil || d != 3*time.Second {
		t.Fatalf("Idle() = %v, %v", d, err)
	}
	if c.Current() != "logind" || mutter.calls != 1 || saver.calls != 1 {
		t.Fatalf("current=%s mutter=%d saver=%d", c.Current(), mutter.calls, saver.calls)
	}

	// Steady state: only the remembered source is called.
	_, _ = c.Idle(t.Context())
	if mutter.calls != 1 || logind.calls != 2 {
		t.Fatalf("steady state re-probed: mutter=%d logind=%d", mutter.calls, logind.calls)
	}

	// After rescanEvery, a better source that came up (Mutter starting late)
	// takes over.
	mutter.err, mutter.idle = nil, time.Second
	now = now.Add(rescanEvery)
	if d, _ := c.Idle(t.Context()); d != time.Second || c.Current() != "mutter" {
		t.Fatalf("upgrade: d=%v current=%s", d, c.Current())
	}

	// When the current source breaks, the chain falls through to the next.
	mutter.err = errDown
	saver.err, saver.idle = nil, 7*time.Second
	if d, _ := c.Idle(t.Context()); d != 7*time.Second || c.Current() != "saver" {
		t.Fatalf("fallback: d=%v current=%s", d, c.Current())
	}

	// Nothing works: unknown, and no re-probing until rescanEvery has passed.
	saver.err, logind.err = errDown, errDown
	if _, err := c.Idle(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("all down: err = %v", err)
	}
	before := mutter.calls + saver.calls + logind.calls
	if _, err := c.Idle(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("all down again: err = %v", err)
	}
	if after := mutter.calls + saver.calls + logind.calls; after != before {
		t.Fatalf("re-probed while backing off: %d calls", after-before)
	}
	if c.Current() != "none" {
		t.Fatalf("current = %s, want none", c.Current())
	}
	now = now.Add(rescanEvery)
	logind.err = nil
	if _, err := c.Idle(t.Context()); err != nil || c.Current() != "logind" {
		t.Fatalf("recovery: err=%v current=%s", err, c.Current())
	}
}

func TestTruncateName(t *testing.T) {
	long := strings.Repeat("a", 255) + "é" // é is 2 bytes, straddling the limit
	if got := truncateName(long); len(got) != 255 {
		t.Fatalf("len = %d, want 255 (rune not split)", len(got))
	}
	if got := truncateName("Code.exe"); got != "Code.exe" {
		t.Fatalf("short name changed: %q", got)
	}
}
