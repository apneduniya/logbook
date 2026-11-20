package localapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatusBarCachesFailures(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()
	f := newFixture(t, upstream.URL)
	clock := time.Now()
	f.nowFunc = func() time.Time { return clock }
	const path = "/api/v1/users/current/statusbar/today"

	for range 3 {
		rec := f.do(t, http.MethodGet, path, nil, basicKey(testKey))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "(offline)") {
			t.Fatalf("fallback = %d %s", rec.Code, rec.Body)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (failure cached)", hits.Load())
	}
	clock = clock.Add(time.Minute)
	f.do(t, http.MethodGet, path, nil, basicKey(testKey))
	if hits.Load() != 2 {
		t.Fatalf("upstream hits after TTL = %d, want 2", hits.Load())
	}
}

func TestStatusBarDoesNotQueueBehindSlowUpstream(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release // a network that drops packets
		_, _ = w.Write([]byte(`{"data":{"grand_total":{"text":"2 hrs","total_seconds":7200}}}`))
	}))
	defer upstream.Close()
	defer close(release)
	f := newFixture(t, upstream.URL)
	const path = "/api/v1/users/current/statusbar/today"

	first := make(chan struct{})
	go func() {
		defer close(first)
		f.do(t, http.MethodGet, path, nil, basicKey(testKey))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first poll never reached upstream")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Other editors polling meanwhile get the local estimate at once.
	for range 3 {
		start := time.Now()
		rec := f.do(t, http.MethodGet, path, nil, basicKey(testKey))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "grand_total") {
			t.Fatalf("concurrent poll = %d %s", rec.Code, rec.Body)
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("concurrent poll waited %v for the upstream request", d)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (single flight)", hits.Load())
	}
	release <- struct{}{}
	<-first
}
