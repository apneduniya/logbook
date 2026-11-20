package hackatime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// These tests run the limiter on its wall clock inside a synctest bubble.
// Time there is virtual: it moves only when every goroutine in the bubble is
// blocked, and synctest.Wait returns once they all are. So "B is asleep in
// Wait" and "B was let through at exactly 1m" are deterministic, sleeps and
// pauses really race the way they do in production, and a goroutine left
// blocked when the test ends fails it.

// waitResult is what one background Wait returned, and when.
type waitResult struct {
	id  int
	err error
	at  time.Duration // since the waiters started
}

// waiters runs Wait calls in the background and collects their results in
// the order they returned.
type waiters struct {
	l     *Limiter
	start time.Time
	wg    sync.WaitGroup

	mu  sync.Mutex
	got []waitResult
}

func newWaiters(l *Limiter) *waiters { return &waiters{l: l, start: time.Now()} }

// add starts waiter id and returns once it is blocked inside Wait (or has
// returned), so waiters queue in the order they were added.
func (w *waiters) add(ctx context.Context, id int) {
	w.wg.Go(func() {
		err := w.l.Wait(ctx)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.got = append(w.got, waitResult{id: id, err: err, at: time.Since(w.start)})
	})
	synctest.Wait()
}

// results returns what the waiters have returned so far.
func (w *waiters) results() []waitResult {
	synctest.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.got)
}

// advanceTo moves virtual time to start+d, letting every waiter whose time
// comes run first.
func (w *waiters) advanceTo(d time.Duration) {
	time.Sleep(time.Until(w.start.Add(d)))
	synctest.Wait()
}

// done waits for every waiter and returns all results.
func (w *waiters) done() []waitResult {
	w.wg.Wait()
	return w.results()
}

// TestLimiterPauseWhileWaiting is the review's counterexample: B holds the
// slot at +500ms when A's request gets a 429 and pauses everyone for 1m.
// B must not go out at +500ms.
func TestLimiterPauseWhileWaiting(t *testing.T) {
	const interval = 500 * time.Millisecond

	t.Run("pause within MaxPause holds the sleeper until it ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := NewLimiter(LimiterConfig{Interval: interval, Burst: 1, MaxPause: 2 * time.Minute})
			w := newWaiters(l)
			if err := l.Wait(t.Context()); err != nil { // A, at 0
				t.Fatal(err)
			}
			w.add(t.Context(), 1) // B, asleep until +500ms
			l.Pause(time.Minute)  // A got a 429

			w.advanceTo(59 * time.Second)
			if got := w.results(); len(got) != 0 {
				t.Fatalf("B returned during the pause: %+v", got)
			}
			want := []waitResult{{id: 1, at: time.Minute}}
			if got := w.done(); !slices.Equal(got, want) {
				t.Fatalf("results = %+v, want %+v", got, want)
			}
		})
	})

	t.Run("pause past MaxPause sends the sleeper away at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := NewLimiter(LimiterConfig{Interval: interval, Burst: 1, MaxPause: 30 * time.Second})
			w := newWaiters(l)
			if err := l.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			w.add(t.Context(), 1)
			l.Pause(time.Minute)

			got := w.results() // no time has passed
			var rl *RateLimitedError
			if len(got) != 1 || !errors.As(got[0].err, &rl) || rl.RetryAfter != time.Minute || got[0].at != 0 {
				t.Fatalf("results = %+v, want B rate limited for 1m straight away", got)
			}
			// B gave its slot up: the next caller goes at the pause's end.
			w.advanceTo(time.Minute)
			w.add(t.Context(), 2)
			if got := w.done(); len(got) != 2 || got[1] != (waitResult{id: 2, at: time.Minute}) {
				t.Fatalf("results = %+v, want caller 2 let through at 1m", got)
			}
		})
	})
}

// TestLimiterPauseRechecksEverySleeper queues callers behind each other and
// pauses while they wait. Each one is held (or sent away) by the pause, and
// they keep their order afterwards.
func TestLimiterPauseRechecksEverySleeper(t *testing.T) {
	const interval = 500 * time.Millisecond
	ms := time.Millisecond

	t.Run("queued callers resume in order at the sustained rate", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := NewLimiter(LimiterConfig{Interval: interval, Burst: 2, MaxPause: 30 * time.Second})
			w := newWaiters(l)
			for id := 1; id <= 5; id++ {
				w.add(t.Context(), id)
			}
			// Burst: 1 and 2 at 0. 3, 4 and 5 are due at 0.5s, 1s and 1.5s.
			l.Pause(10 * time.Second)
			w.advanceTo(9 * time.Second)
			w.add(t.Context(), 6) // asks during the pause, behind 3, 4 and 5

			want := []waitResult{
				{id: 1, at: 0}, {id: 2, at: 0},
				{id: 3, at: 10 * time.Second},
				{id: 4, at: 10*time.Second + 500*ms},
				{id: 5, at: 11 * time.Second},
				{id: 6, at: 11*time.Second + 500*ms},
			}
			if got := w.done(); !slices.Equal(got, want) {
				t.Fatalf("results = %+v, want %+v", got, want)
			}
		})
	})

	t.Run("a long pause sends every queued caller away", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := NewLimiter(LimiterConfig{Interval: interval, Burst: 1, MaxPause: 30 * time.Second})
			w := newWaiters(l)
			for id := 1; id <= 4; id++ {
				w.add(t.Context(), id)
			}
			w.advanceTo(100 * ms) // 1 went at 0; 2, 3, 4 are due at 0.5s, 1s, 1.5s
			l.Pause(time.Minute)

			got := w.done()
			if len(got) != 4 || got[0] != (waitResult{id: 1, at: 0}) {
				t.Fatalf("results = %+v, want 1 let through at 0", got)
			}
			for _, r := range got[1:] {
				var rl *RateLimitedError
				if !errors.As(r.err, &rl) || rl.RetryAfter != time.Minute || r.at != 100*ms {
					t.Fatalf("results = %+v, want 2, 3 and 4 rate limited for 1m at 100ms", got)
				}
			}
		})
	})
}

// TestLimiterPauseExtended pauses again while callers wait out a pause.
func TestLimiterPauseExtended(t *testing.T) {
	const interval = 500 * time.Millisecond

	t.Run("within MaxPause the caller waits for the later end", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := NewLimiter(LimiterConfig{Interval: interval, Burst: 1, MaxPause: 30 * time.Second})
			w := newWaiters(l)
			l.Pause(10 * time.Second)
			w.add(t.Context(), 1)
			w.advanceTo(5 * time.Second)
			l.Pause(20 * time.Second) // now ends at 25s
			l.Pause(time.Second)      // a shorter overlapping pause changes nothing

			want := []waitResult{{id: 1, at: 25 * time.Second}}
			if got := w.done(); !slices.Equal(got, want) {
				t.Fatalf("results = %+v, want %+v", got, want)
			}
		})
	})

	t.Run("past MaxPause the caller is sent away when it grows", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := NewLimiter(LimiterConfig{Interval: interval, Burst: 1, MaxPause: 30 * time.Second})
			w := newWaiters(l)
			l.Pause(10 * time.Second)
			w.add(t.Context(), 1)
			w.advanceTo(5 * time.Second)
			l.Pause(40 * time.Second)

			got := w.done()
			var rl *RateLimitedError
			if len(got) != 1 || !errors.As(got[0].err, &rl) || rl.RetryAfter != 40*time.Second || got[0].at != 5*time.Second {
				t.Fatalf("results = %+v, want rate limited for 40s at 5s", got)
			}
		})
	})
}

// TestLimiterCancelDuringPause cancels callers waiting out a pause. They
// return at once with ctx's error, and the caller behind them moves up.
func TestLimiterCancelDuringPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewLimiter(LimiterConfig{Interval: 500 * time.Millisecond, Burst: 1, MaxPause: 30 * time.Second})
		w := newWaiters(l)
		l.Pause(10 * time.Second)
		ctx1, cancel1 := context.WithCancel(t.Context())
		ctx2, cancel2 := context.WithCancel(t.Context())
		w.add(ctx1, 1) // front of the queue, sleeping until 10s
		w.add(ctx2, 2) // waiting behind 1
		w.add(t.Context(), 3)
		w.advanceTo(3 * time.Second)
		cancel1()
		cancel2()

		got := w.done()
		if len(got) != 3 {
			t.Fatalf("results = %+v, want 3", got)
		}
		for _, r := range got[:2] {
			if !errors.Is(r.err, context.Canceled) || r.at != 3*time.Second {
				t.Fatalf("results = %+v, want 1 and 2 cancelled at 3s", got)
			}
		}
		if got[2] != (waitResult{id: 3, at: 10 * time.Second}) {
			t.Fatalf("results = %+v, want 3 let through at 10s", got)
		}
	})
}
