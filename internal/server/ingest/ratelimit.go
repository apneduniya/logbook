package ingest

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Default per-user request limits for the heartbeat endpoints. wakatime-cli
// sends one request per heartbeat command and drains its offline queue in
// requests of 10, up to 100 requests in a burst; a rejected request is
// re-queued by the CLI and sent later, so nothing is lost.
const (
	defaultRatePerSecond = 2
	defaultBurst         = 120
)

// limiter is an in-memory token bucket per user. Limits are per server
// process; that's enough to stop one key flooding one instance.
type limiter struct {
	mu        sync.Mutex
	rate      float64 // tokens added per second
	burst     float64
	now       func() time.Time
	buckets   map[uuid.UUID]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64, now func() time.Time) *limiter {
	return &limiter{rate: rate, burst: burst, now: now, buckets: map[uuid.UUID]*bucket{}}
}

// allow takes one token from userID's bucket and reports whether it had one.
func (l *limiter) allow(userID uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[userID]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[userID] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have refilled completely, at most once a minute,
// so memory tracks active users only. The caller holds l.mu.
func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for id, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, id)
		}
	}
}
