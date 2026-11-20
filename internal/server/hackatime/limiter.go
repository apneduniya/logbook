package hackatime

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Clock is the time source a Limiter waits on. Tests inject a virtual clock
// so rate tests are fast and deterministic.
type Clock interface {
	Now() time.Time
	// SleepUntil returns nil at t or as soon as wake is closed, whichever
	// comes first, or early with ctx's error. The Limiter calls it once
	// before letting each request through, even when no wait is needed, and
	// again whenever it has to wait longer than it planned.
	SleepUntil(ctx context.Context, t time.Time, wake <-chan struct{}) error
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) SleepUntil(ctx context.Context, t time.Time, wake <-chan struct{}) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	case <-timer.C:
		return nil
	}
}

// LimiterConfig configures a Limiter. Zero values get defaults.
type LimiterConfig struct {
	// Interval is the sustained spacing between requests. Default 500ms
	// (2 requests a second, 7,200 an hour: under Hackatime's 10,000 an hour
	// per IP, which every Logbook user shares).
	Interval time.Duration
	// Burst is how many requests may go out back to back after a quiet
	// spell. Default 2.
	Burst int
	// MaxPause is the longest a caller waits out a rate-limit pause; past it,
	// Wait returns *RateLimitedError so the job can snooze instead of holding
	// a worker. Default 30s.
	MaxPause time.Duration
	// Clock defaults to the wall clock.
	Clock Clock
}

// Limiter is the one budget for every Hackatime request this process makes:
// imports, trust refreshes and OAuth calls all wait on it. It is a token
// bucket (GCRA): at most Burst requests at once, then one per Interval.
//
// Callers queue in the order they ask, and only the caller at the front
// waits for the next slot, so callers that each keep a fixed number of
// requests in flight (the importer's per-import concurrency) share the
// budget evenly; none can starve another. A 429 from Hackatime pauses every
// caller (Pause), including those already waiting, and after the pause
// requests resume at the sustained rate, without a burst.
type Limiter struct {
	interval time.Duration
	tau      time.Duration // (burst-1) * interval: how far ahead of schedule a request may go
	maxPause time.Duration
	clock    Clock

	mu      sync.Mutex
	tat     time.Time     // theoretical arrival time of the next request
	paused  time.Time     // no request goes out before this
	queue   list.List     // callers waiting in Wait, oldest first
	changed chan struct{} // closed, then replaced, when a pause starts or the front of the queue leaves
}

// NewLimiter builds a Limiter.
func NewLimiter(cfg LimiterConfig) *Limiter {
	l := &Limiter{
		interval: orDefault(cfg.Interval, 500*time.Millisecond),
		maxPause: orDefault(cfg.MaxPause, 30*time.Second),
		clock:    cfg.Clock,
		changed:  make(chan struct{}),
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = 2
	}
	l.tau = time.Duration(burst-1) * l.interval
	if l.clock == nil {
		l.clock = realClock{}
	}
	return l
}

// Wait blocks until the caller may send one request. If Hackatime asked
// everyone to pause for longer than MaxPause, it returns *RateLimitedError
// at once without using a slot, also when that pause starts while the
// caller is waiting.
//
// The caller joins the back of the queue. The front of the queue sleeps
// until its slot; everyone else waits for the front to leave. Any pause
// wakes them all to check the new state, so nobody goes out on a slot
// worked out before the pause.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	me := l.queue.PushBack(nil)
	defer l.leave(me)

	slept := false // the front has been through SleepUntil since it last looked
	for {
		now := l.clock.Now()
		if left := l.paused.Sub(now); left > l.maxPause {
			return &RateLimitedError{RetryAfter: left}
		}
		front := l.queue.Front() == me
		at := latest(latest(now, l.tat.Add(-l.tau)), l.paused)
		if front && slept && !at.After(now) {
			l.tat = latest(l.tat, at).Add(l.interval)
			return nil
		}

		changed := l.changed
		l.mu.Unlock()
		var err error
		if front {
			err = l.clock.SleepUntil(ctx, at, changed)
			slept = true
		} else {
			select {
			case <-changed:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		l.mu.Lock()
		if err != nil {
			return err
		}
	}
}

// leave takes a caller out of the queue. When it was at the front, the
// callers behind it are woken so the next one can take its turn. l.mu must
// be held.
func (l *Limiter) leave(e *list.Element) {
	front := l.queue.Front() == e
	l.queue.Remove(e)
	if front {
		l.wakeAll()
	}
}

// wakeAll wakes every caller waiting in Wait to look at the state again.
// l.mu must be held.
func (l *Limiter) wakeAll() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// Pause stops every caller from sending until now+d, after Hackatime
// answered 429. Overlapping pauses keep the later end. Callers already
// waiting are woken: each either waits for the pause to end or, if that is
// more than MaxPause away, returns *RateLimitedError.
func (l *Limiter) Pause(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until := l.clock.Now().Add(d)
	if !until.After(l.paused) {
		return
	}
	l.paused = until
	// Resume at the sustained rate: the first request may go at the end of
	// the pause, the next one an interval later.
	l.tat = latest(l.tat, until.Add(l.tau))
	l.wakeAll()
}
