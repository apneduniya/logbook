package live

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/stats/statstest"
)

type fakeLookup map[string]uuid.UUID

func (f fakeLookup) PublicUserID(_ context.Context, username string) (uuid.UUID, bool, error) {
	id, ok := f[username]
	return id, ok, nil
}

type sseEvent struct {
	name, data string
	comment    bool
}

// client is one open SSE stream read line by line on its own goroutine.
type client struct {
	resp   *http.Response
	cancel context.CancelFunc
	events chan sseEvent
}

func open(t *testing.T, srv *httptest.Server, path string, user uuid.UUID) (*client, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != uuid.Nil {
		req.Header.Set(statstest.UserHeader, user.String())
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		return nil, resp.StatusCode
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	c := &client{resp: resp, cancel: cancel, events: make(chan sseEvent, 64)}
	go c.read()
	t.Cleanup(c.close)
	return c, resp.StatusCode
}

func (c *client) read() {
	defer close(c.events)
	sc := bufio.NewScanner(c.resp.Body)
	var ev sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev != (sseEvent{}) {
				c.events <- ev
			}
			ev = sseEvent{}
		case strings.HasPrefix(line, ":"):
			ev.comment = true
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func (c *client) close() {
	c.cancel()
	_ = c.resp.Body.Close()
}

func (c *client) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-c.events:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return sseEvent{}
}

func (c *client) waitEnd(t *testing.T) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.events:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("stream did not end")
		}
	}
}

func newServer(t *testing.T, hub *Hub, lookup fakeLookup, cfg Config) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewHandler(hub, lookup, statstest.StubSession, cfg).Register(mux)
	// Wrap like production so flushing goes through the logging recorder.
	srv := httptest.NewServer(httpapi.Chain(mux, httpapi.Recover, httpapi.RequestID, httpapi.Logger))
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOwnerStreamDeliveryAndIsolation(t *testing.T) {
	hub := NewHub(DefaultLimits())
	srv := newServer(t, hub, fakeLookup{}, Config{})
	a, b := uuid.New(), uuid.New()

	ca, _ := open(t, srv, "/api/v1/me/live", a)
	cb, _ := open(t, srv, "/api/v1/me/live", b)
	for _, c := range []*client{ca, cb} {
		if ev := c.next(t); ev.name != "ready" {
			t.Fatalf("first event = %+v", ev)
		}
	}
	waitFor(t, "two subscriptions", func() bool { return hub.Count() == 2 })

	hub.MinutesChanged(b, t0.Add(time.Hour), t0.Add(2*time.Hour))
	hub.MinutesChanged(a, t0, t0.Add(time.Minute))

	ev := ca.next(t)
	var got minutesData
	if err := json.Unmarshal([]byte(ev.data), &got); err != nil || ev.name != "minutes" {
		t.Fatalf("event = %+v (%v)", ev, err)
	}
	if !got.From.Equal(t0) || !got.To.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a received %+v; b's range leaked or range wrong", got)
	}
	ev = cb.next(t)
	if err := json.Unmarshal([]byte(ev.data), &got); err != nil || !got.From.Equal(t0.Add(time.Hour)) {
		t.Fatalf("b received %+v", ev)
	}

	if _, code := open(t, srv, "/api/v1/me/live", uuid.Nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", code)
	}
}

func TestPublicStream(t *testing.T) {
	hub := NewHub(DefaultLimits())
	owner := uuid.New()
	srv := newServer(t, hub, fakeLookup{"alice": owner}, Config{})

	if _, code := open(t, srv, "/api/v1/users/bob/live", uuid.Nil); code != http.StatusNotFound {
		t.Fatalf("private/unknown profile status = %d, want 404", code)
	}
	c, _ := open(t, srv, "/api/v1/users/alice/live", uuid.Nil)
	c.next(t) // ready
	waitFor(t, "subscription", func() bool { return hub.Count() == 1 })

	hub.MinutesChanged(owner, t0, t0.Add(time.Minute))
	if ev := c.next(t); ev.name != "minutes" || ev.data != "{}" {
		t.Fatalf("public event = %+v; must carry no data", ev)
	}

	hub.ProfileMadePrivate(owner)
	c.waitEnd(t)
	waitFor(t, "unsubscribe", func() bool { return hub.Count() == 0 })
}

func TestKeepaliveAndMaxDuration(t *testing.T) {
	hub := NewHub(DefaultLimits())
	srv := newServer(t, hub, fakeLookup{}, Config{Keepalive: 20 * time.Millisecond, MaxDuration: 300 * time.Millisecond})
	c, _ := open(t, srv, "/api/v1/me/live", uuid.New())
	c.next(t) // ready
	if ev := c.next(t); !ev.comment {
		t.Fatalf("expected keepalive comment, got %+v", ev)
	}
	c.waitEnd(t)
	waitFor(t, "unsubscribe after max duration", func() bool { return hub.Count() == 0 })
}

func TestStreamCaps(t *testing.T) {
	hub := NewHub(Limits{PerUser: 1, PerProfile: 1, Global: 3})
	owner := uuid.New()
	srv := newServer(t, hub, fakeLookup{"alice": owner}, Config{})

	open(t, srv, "/api/v1/me/live", owner)
	if _, code := open(t, srv, "/api/v1/me/live", owner); code != http.StatusTooManyRequests {
		t.Fatalf("second own stream status = %d, want 429", code)
	}
	open(t, srv, "/api/v1/users/alice/live", uuid.Nil)
	if _, code := open(t, srv, "/api/v1/users/alice/live", uuid.Nil); code != http.StatusTooManyRequests {
		t.Fatalf("second public stream status = %d, want 429", code)
	}
	open(t, srv, "/api/v1/me/live", uuid.New())
	if _, code := open(t, srv, "/api/v1/me/live", uuid.New()); code != http.StatusServiceUnavailable {
		t.Fatalf("over global cap status = %d, want 503", code)
	}
}

func TestDisconnectCleansUpWithoutLeaks(t *testing.T) {
	before := runtime.NumGoroutine()
	hub := NewHub(DefaultLimits())
	mux := http.NewServeMux()
	NewHandler(hub, fakeLookup{}, statstest.StubSession, Config{Keepalive: 10 * time.Millisecond}).Register(mux)
	srv := httptest.NewServer(mux)

	clients := make([]*client, 0, 20)
	for range 20 {
		u := uuid.New()
		c, _ := open(t, srv, "/api/v1/me/live", u)
		c.next(t)
		hub.MinutesChanged(u, t0, t0.Add(time.Minute))
		clients = append(clients, c)
	}
	waitFor(t, "20 subscriptions", func() bool { return hub.Count() == 20 })
	for _, c := range clients {
		c.close()
	}
	waitFor(t, "all unsubscribed", func() bool { return hub.Count() == 0 })

	srv.CloseClientConnections()
	srv.Close()
	srv.Client().CloseIdleConnections()
	waitFor(t, "goroutines to return to baseline", func() bool { return runtime.NumGoroutine() <= before })
}

func TestHubCloseEndsStreams(t *testing.T) {
	hub := NewHub(DefaultLimits())
	srv := newServer(t, hub, fakeLookup{}, Config{})
	c, _ := open(t, srv, "/api/v1/me/live", uuid.New())
	c.next(t)
	hub.Close()
	c.waitEnd(t)
	if _, code := open(t, srv, "/api/v1/me/live", uuid.New()); code != http.StatusServiceUnavailable {
		t.Fatalf("after Close status = %d, want 503", code)
	}
}
