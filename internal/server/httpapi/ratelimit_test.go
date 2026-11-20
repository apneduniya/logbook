package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestLimiter(perSecond float64, burst, maxKeys int, ttl time.Duration) (*RateLimiter, *time.Time) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewRateLimiter(perSecond, burst, maxKeys, ttl)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestRateLimiterBurstAndRefill(t *testing.T) {
	l, now := newTestLimiter(1, 3, 10, time.Hour)
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("request %d denied within burst", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("request beyond burst allowed")
	}
	if !l.Exhausted("a") || l.RetryAfter("a") != time.Second {
		t.Fatalf("Exhausted=%v RetryAfter=%v", l.Exhausted("a"), l.RetryAfter("a"))
	}
	if !l.Allow("b") {
		t.Fatal("independent key denied")
	}
	*now = now.Add(time.Second)
	if !l.Allow("a") {
		t.Fatal("not refilled after 1s")
	}
}

func TestRateLimiterBoundedMemory(t *testing.T) {
	l, _ := newTestLimiter(1, 1, 100, time.Hour)
	for i := range 1000 {
		l.Allow(fmt.Sprintf("k%d", i))
	}
	if n := l.Len(); n != 100 {
		t.Fatalf("Len = %d, want 100", n)
	}
	// The most recent key survived; the oldest was evicted (fresh bucket).
	if l.Allow("k999") {
		t.Fatal("recent key lost its state")
	}
	if !l.Allow("k0") {
		t.Fatal("evicted key should start with a full bucket")
	}
}

func TestRateLimiterEvictsIdle(t *testing.T) {
	l, now := newTestLimiter(1, 1, 100, time.Minute)
	for i := range 10 {
		l.Allow(fmt.Sprintf("k%d", i))
	}
	*now = now.Add(2 * time.Minute)
	l.Allow("fresh")
	if n := l.Len(); n != 1 {
		t.Fatalf("Len = %d after idle eviction, want 1", n)
	}
}

func TestRateLimiterMiddleware(t *testing.T) {
	l, _ := newTestLimiter(1, 2, 10, time.Hour)
	h := l.Middleware(IPKey(false))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	codes := make([]int, 0, 3)
	for range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
		codes = append(codes, rec.Code)
		if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "1" {
			t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
		}
	}
	if fmt.Sprint(codes) != "[204 204 429]" {
		t.Fatalf("codes = %v", codes)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name, remote string
		xff          []string
		trust        bool
		want         string
	}{
		{"remote addr", "192.0.2.1:1234", nil, false, "192.0.2.1"},
		{"ipv6 remote", "[2001:db8::1]:443", nil, false, "2001:db8::1"},
		{"xff ignored without trust", "10.0.0.1:1", []string{"203.0.113.9"}, false, "10.0.0.1"},
		{"xff last hop with trust", "10.0.0.1:1", []string{"1.2.3.4, 203.0.113.9"}, true, "203.0.113.9"},
		{"xff spoofed first entry", "10.0.0.1:1", []string{"6.6.6.6", "203.0.113.9"}, true, "203.0.113.9"},
		{"xff garbage falls back", "10.0.0.1:1", []string{"not-an-ip"}, true, "10.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r, tc.trust); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
