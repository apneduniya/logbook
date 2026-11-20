package hackatime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// virtualClock is a Clock whose sleeps return at once and move virtual time
// forward. It records every time the limiter let a request go, so tests can
// check the request rate without waiting for it.
type virtualClock struct {
	mu       sync.Mutex
	now      time.Time
	admitted []time.Time
}

func newVirtualClock() *virtualClock {
	return &virtualClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *virtualClock) SleepUntil(ctx context.Context, t time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.admitted = append(c.admitted, t)
	c.now = latest(c.now, t)
	return nil
}

func (c *virtualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// admissions returns when each request was let through, relative to start.
func (c *virtualClock) admissions(start time.Time) []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.admitted))
	for i, t := range c.admitted {
		out[i] = t.Sub(start)
	}
	return out
}

// checkBucket fails if any run of admissions (in the order they were let
// through) beats a token bucket of burst and interval.
func checkBucket(t *testing.T, adm []time.Duration, burst int, interval time.Duration) {
	t.Helper()
	for i := range adm {
		for j := i + 1; j < len(adm); j++ {
			if adm[j] < adm[i] {
				t.Fatalf("admission %d at %v is before admission %d at %v", j, adm[j], i, adm[i])
			}
			if n, allowed := j-i+1, burst+int((adm[j]-adm[i])/interval); n > allowed {
				t.Fatalf("%d requests between %v and %v, bucket allows %d", n, adm[i], adm[j], allowed)
			}
		}
	}
}

func TestLimiter(t *testing.T) {
	ctx := context.Background()
	const interval = 500 * time.Millisecond
	ms := time.Millisecond

	t.Run("burst then sustained rate", func(t *testing.T) {
		vc := newVirtualClock()
		start := vc.Now()
		l := NewLimiter(LimiterConfig{Interval: interval, Burst: 2, Clock: vc})
		for range 6 {
			if err := l.Wait(ctx); err != nil {
				t.Fatal(err)
			}
		}
		want := []time.Duration{0, 0, 500 * ms, 1000 * ms, 1500 * ms, 2000 * ms}
		if got := vc.admissions(start); !slices.Equal(got, want) {
			t.Fatalf("admissions = %v, want %v", got, want)
		}
	})

	t.Run("pause holds everyone and resumes without a burst", func(t *testing.T) {
		vc := newVirtualClock()
		start := vc.Now()
		l := NewLimiter(LimiterConfig{Interval: interval, Burst: 2, MaxPause: 30 * time.Second, Clock: vc})
		l.Pause(10 * time.Second)
		for range 3 {
			if err := l.Wait(ctx); err != nil {
				t.Fatal(err)
			}
		}
		want := []time.Duration{10 * time.Second, 10*time.Second + 500*ms, 11 * time.Second}
		if got := vc.admissions(start); !slices.Equal(got, want) {
			t.Fatalf("admissions = %v, want %v", got, want)
		}
	})

	t.Run("long pause is returned to the caller", func(t *testing.T) {
		vc := newVirtualClock()
		l := NewLimiter(LimiterConfig{Interval: interval, MaxPause: 30 * time.Second, Clock: vc})
		l.Pause(10 * time.Minute)
		var rl *RateLimitedError
		if err := l.Wait(ctx); !errors.As(err, &rl) || rl.RetryAfter != 10*time.Minute {
			t.Fatalf("Wait() = %v, want rate limited for 10m", err)
		}
		vc.advance(10 * time.Minute)
		if err := l.Wait(ctx); err != nil {
			t.Fatalf("Wait() after the pause = %v", err)
		}
		if adm := vc.admissions(vc.Now()); len(adm) != 1 || adm[0] != 0 {
			t.Fatalf("admissions = %v; the refused wait must not use a slot", adm)
		}
	})

}
