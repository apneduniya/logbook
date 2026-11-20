package httpapi

import (
	"container/list"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter is a set of per-key token buckets with bounded memory. It keeps
// at most maxKeys buckets, evicting the least recently used one when full and
// dropping buckets that have been idle longer than idleTTL.
type RateLimiter struct {
	limit   rate.Limit
	burst   int
	maxKeys int
	idleTTL time.Duration
	now     func() time.Time

	mu    sync.Mutex
	order *list.List               // front = most recently used
	items map[string]*list.Element // value is *bucket
}

type bucket struct {
	key      string
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter allows each key `burst` requests at once, refilled at
// `perSecond` tokens per second.
func NewRateLimiter(perSecond float64, burst, maxKeys int, idleTTL time.Duration) *RateLimiter {
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &RateLimiter{
		limit:   rate.Limit(perSecond),
		burst:   burst,
		maxKeys: maxKeys,
		idleTTL: idleTTL,
		now:     time.Now,
		order:   list.New(),
		items:   make(map[string]*list.Element),
	}
}

// Allow takes one token for key and reports whether one was available.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	return l.get(key, now).limiter.AllowN(now, 1)
}

// Exhausted reports whether key has no token left, without taking one.
func (l *RateLimiter) Exhausted(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	return l.get(key, now).limiter.TokensAt(now) < 1
}

// RetryAfter is how long until key has a token again, rounded up to seconds.
func (l *RateLimiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	tokens := l.get(key, now).limiter.TokensAt(now)
	if tokens >= 1 || l.limit <= 0 {
		return 0
	}
	secs := math.Ceil((1 - tokens) / float64(l.limit))
	return time.Duration(secs) * time.Second
}

// Len returns the number of tracked keys (for tests and metrics).
func (l *RateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}

// get returns the bucket for key, creating it and evicting old ones as
// needed. The caller holds l.mu.
func (l *RateLimiter) get(key string, now time.Time) *bucket {
	l.evictIdle(now)
	if el, ok := l.items[key]; ok {
		b := el.Value.(*bucket)
		b.lastSeen = now
		l.order.MoveToFront(el)
		return b
	}
	for len(l.items) >= l.maxKeys {
		l.remove(l.order.Back())
	}
	b := &bucket{key: key, limiter: rate.NewLimiter(l.limit, l.burst), lastSeen: now}
	l.items[key] = l.order.PushFront(b)
	return b
}

// evictIdle drops buckets unused for idleTTL, oldest first. A bucket idle
// that long has refilled for any sensible limit, so dropping it is lossless.
func (l *RateLimiter) evictIdle(now time.Time) {
	if l.idleTTL <= 0 {
		return
	}
	for el := l.order.Back(); el != nil; el = l.order.Back() {
		b := el.Value.(*bucket)
		if now.Sub(b.lastSeen) < l.idleTTL {
			return
		}
		l.remove(el)
	}
}

func (l *RateLimiter) remove(el *list.Element) {
	b := l.order.Remove(el).(*bucket)
	delete(l.items, b.key)
}

// Middleware rejects requests over the limit with 429 and a Retry-After
// header. key extracts the bucket key; an empty key is not limited.
func (l *RateLimiter) Middleware(key func(*http.Request) string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := key(r)
			if k != "" && !l.Allow(k) {
				WriteTooManyRequests(w, r, l.RetryAfter(k))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteTooManyRequests writes a 429 JSON error with Retry-After.
func WriteTooManyRequests(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	secs := int(retryAfter / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	WriteError(w, r, Errorf(http.StatusTooManyRequests, nil, "too many requests"))
}

// ClientIP returns the caller's IP address. With trustProxy, it uses the
// last X-Forwarded-For entry, which is the address the reverse proxy saw;
// earlier entries are client-controlled and ignored.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			last := xff[len(xff)-1]
			if i := strings.LastIndexByte(last, ','); i >= 0 {
				last = last[i+1:]
			}
			if ip := net.ParseIP(strings.TrimSpace(last)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// IPKey returns a key function for Middleware that limits per client IP.
func IPKey(trustProxy bool) func(*http.Request) string {
	return func(r *http.Request) string { return ClientIP(r, trustProxy) }
}
