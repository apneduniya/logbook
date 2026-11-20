package live

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
)

// fakeProfiles is an in-memory PublicProfiles. names resolve in
// PublicUserID whenever the user is public at lookup time; PublicVersion
// reads public and version at call time.
type fakeProfiles struct {
	mu      sync.Mutex
	names   map[string]uuid.UUID
	public  map[uuid.UUID]bool
	version map[uuid.UUID]string
	checks  int
	// hideOnLookup makes the profile private right after PublicUserID
	// answers, as a concurrent PATCH would.
	hideOnLookup bool
}

func newFakeProfiles() *fakeProfiles {
	return &fakeProfiles{names: map[string]uuid.UUID{}, public: map[uuid.UUID]bool{}, version: map[uuid.UUID]string{}}
}

func (f *fakeProfiles) add(name string, public bool) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	f.names[name], f.public[id], f.version[id] = id, public, "v0"
	return id
}

func (f *fakeProfiles) setVersion(id uuid.UUID, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version[id] = v
}

func (f *fakeProfiles) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

func (f *fakeProfiles) PublicUserID(_ context.Context, username string) (uuid.UUID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.names[username]
	ok = ok && f.public[id]
	if ok && f.hideOnLookup {
		f.public[id] = false
	}
	return id, ok, nil
}

func (f *fakeProfiles) PublicVersion(_ context.Context, id uuid.UUID, _ time.Time) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.version[id], f.public[id], nil
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

// clientHeader carries the fake client IP read by testClientKey.
const clientHeader = "X-Test-Client"

func testClientKey(r *http.Request) string { return r.Header.Get(clientHeader) }

func open(t *testing.T, srv *httptest.Server, path string, user uuid.UUID, clientIP ...string) (*client, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != uuid.Nil {
		req.Header.Set(httpapitest.UserHeader, user.String())
	}
	if len(clientIP) > 0 {
		req.Header.Set(clientHeader, clientIP[0])
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

// quiet fails if any event other than a keepalive arrives within d.
func (c *client) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				t.Fatal("stream ended")
			}
			if !ev.comment {
				t.Fatalf("unexpected event %+v", ev)
			}
		case <-timeout:
			return
		}
	}
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

func newServer(t *testing.T, hub *Hub, profiles PublicProfiles, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.ClientKey == nil {
		cfg.ClientKey = testClientKey
	}
	mux := http.NewServeMux()
	NewHandler(hub, profiles, httpapitest.StubSession, httpapitest.NoLimit, cfg).Register(mux)
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
	srv := newServer(t, hub, newFakeProfiles(), Config{})
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

func TestPublicStreamSendsOnlyVisibleChanges(t *testing.T) {
	hub := NewHub(DefaultLimits())
	profiles := newFakeProfiles()
	owner := profiles.add("alice", true)
	profiles.add("bob", false)
	srv := newServer(t, hub, profiles, Config{PublicInterval: 20 * time.Millisecond})

	for _, name := range []string{"bob", "nobody"} {
		if _, code := open(t, srv, "/api/v1/users/"+name+"/live", uuid.Nil, "ip1"); code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", name, code)
		}
	}
	c, _ := open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip1")
	c.next(t) // ready
	waitFor(t, "subscription", func() bool { return hub.Count() == 1 })

	// A change the public profile doesn't show (hidden project, agent time)
	// leaves the version alone: no event.
	hub.MinutesChanged(owner, t0, t0.Add(time.Minute))
	c.quiet(t, 200*time.Millisecond)

	// A visible change sends one empty event.
	profiles.setVersion(owner, "v1")
	hub.MinutesChanged(owner, t0, t0.Add(time.Minute))
	if ev := c.next(t); ev.name != "minutes" || ev.data != "{}" {
		t.Fatalf("public event = %+v; must carry no data", ev)
	}

	hub.ProfileMadePrivate(owner)
	c.waitEnd(t)
	waitFor(t, "unsubscribe", func() bool { return hub.Count() == 0 })
}

func TestPublicStreamIsThrottled(t *testing.T) {
	hub := NewHub(DefaultLimits())
	profiles := newFakeProfiles()
	owner := profiles.add("alice", true)
	srv := newServer(t, hub, profiles, Config{PublicInterval: 400 * time.Millisecond})
	c, _ := open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip1")
	c.next(t)
	waitFor(t, "subscription", func() bool { return hub.Count() == 1 })
	base := profiles.checkCount() // the visibility check made on subscribe

	start := time.Now()
	for i := range 50 {
		profiles.setVersion(owner, "v"+string(rune('a'+i%26)))
		hub.MinutesChanged(owner, t0, t0.Add(time.Minute))
		time.Sleep(time.Millisecond)
	}
	if ev := c.next(t); ev.name != "minutes" {
		t.Fatalf("event = %+v", ev)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("event after %v, want the interval to pass first", waited)
	}
	if n := profiles.checkCount() - base; n != 1 {
		t.Fatalf("%d checks for a burst of 50 changes, want 1", n)
	}
}

func TestPublicStreamEndsWhenPrivateOnCheck(t *testing.T) {
	hub := NewHub(DefaultLimits())
	profiles := newFakeProfiles()
	owner := profiles.add("alice", true)
	srv := newServer(t, hub, profiles, Config{PublicInterval: 10 * time.Millisecond})
	c, _ := open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip1")
	c.next(t)
	profiles.mu.Lock()
	profiles.public[owner] = false
	profiles.mu.Unlock()
	hub.MinutesChanged(owner, t0, t0.Add(time.Minute))
	c.waitEnd(t)
	waitFor(t, "unsubscribe", func() bool { return hub.Count() == 0 })
}

// A profile made private between the lookup and Subscribe misses
// ProfileMadePrivate; the re-check after subscribing must catch it.
func TestPublicStreamRecheckAfterSubscribe(t *testing.T) {
	hub := NewHub(DefaultLimits())
	profiles := newFakeProfiles()
	profiles.add("alice", true)
	profiles.hideOnLookup = true
	srv := newServer(t, hub, profiles, Config{})
	if _, code := open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip1"); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if hub.Count() != 0 {
		t.Fatalf("Count() = %d, subscription leaked", hub.Count())
	}
}

func TestPublicStreamUsesRateLimiter(t *testing.T) {
	hub := NewHub(DefaultLimits())
	profiles := newFakeProfiles()
	profiles.add("alice", true)
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
	mux := http.NewServeMux()
	NewHandler(hub, profiles, httpapitest.StubSession, deny, Config{}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if _, code := open(t, srv, "/api/v1/users/alice/live", uuid.Nil); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", code)
	}
	if c, code := open(t, srv, "/api/v1/me/live", uuid.New()); code != http.StatusOK {
		t.Fatalf("owner stream status = %d; the public limiter must not apply", code)
	} else {
		c.close()
	}
}

func TestKeepaliveAndMaxDuration(t *testing.T) {
	hub := NewHub(DefaultLimits())
	srv := newServer(t, hub, newFakeProfiles(), Config{Keepalive: 20 * time.Millisecond, MaxDuration: 300 * time.Millisecond})
	c, _ := open(t, srv, "/api/v1/me/live", uuid.New())
	c.next(t) // ready
	if ev := c.next(t); !ev.comment {
		t.Fatalf("expected keepalive comment, got %+v", ev)
	}
	c.waitEnd(t)
	waitFor(t, "unsubscribe after max duration", func() bool { return hub.Count() == 0 })
}

func TestStreamCaps(t *testing.T) {
	hub := NewHub(Limits{PerUser: 1, Owner: 2, PerProfile: 2, PerClient: 2, Public: 4})
	profiles := newFakeProfiles()
	alice := profiles.add("alice", true)
	profiles.add("bob", true)
	srv := newServer(t, hub, profiles, Config{})

	open(t, srv, "/api/v1/me/live", alice)
	if _, code := open(t, srv, "/api/v1/me/live", alice); code != http.StatusTooManyRequests {
		t.Fatalf("second own stream status = %d, want 429", code)
	}

	// Per client: one IP can't hold more than PerClient public streams.
	open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip1")
	open(t, srv, "/api/v1/users/bob/live", uuid.Nil, "ip1")
	if _, code := open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip1"); code != http.StatusTooManyRequests {
		t.Fatalf("third stream from one client status = %d, want 429", code)
	}
	// Per profile.
	open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip2")
	if _, code := open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip3"); code != http.StatusTooManyRequests {
		t.Fatalf("third viewer of alice status = %d, want 429", code)
	}
	// Fill the public global cap (4); that must not affect owners.
	open(t, srv, "/api/v1/users/bob/live", uuid.Nil, "ip4")
	if _, code := open(t, srv, "/api/v1/users/bob/live", uuid.Nil, "ip5"); code != http.StatusServiceUnavailable {
		t.Fatalf("over public cap status = %d, want 503", code)
	}
	open(t, srv, "/api/v1/me/live", uuid.New())
	if _, code := open(t, srv, "/api/v1/me/live", uuid.New()); code != http.StatusServiceUnavailable {
		t.Fatalf("over owner cap status = %d, want 503", code)
	}
}

func TestPublicStreamsCannotExhaustOwnerStreams(t *testing.T) {
	limits := Limits{PerUser: 5, Owner: 5, PerProfile: 100, PerClient: 100, Public: 10}
	hub := NewHub(limits)
	profiles := newFakeProfiles()
	profiles.add("alice", true)
	srv := newServer(t, hub, profiles, Config{})
	for i := range limits.Public + 5 {
		open(t, srv, "/api/v1/users/alice/live", uuid.Nil, "ip"+string(rune('a'+i))) // some fail with 503
	}
	for range limits.Owner {
		if _, code := open(t, srv, "/api/v1/me/live", uuid.New()); code != http.StatusOK {
			t.Fatalf("owner stream status = %d with public streams at their cap", code)
		}
	}
}

func TestDefaultClientKeyIsRemoteIP(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.7:51234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := (Config{}).withDefaults().ClientKey(r); got != "198.51.100.7" {
		t.Fatalf("ClientKey = %q; must ignore client-controlled headers by default", got)
	}
}

func TestDisconnectCleansUpWithoutLeaks(t *testing.T) {
	before := runtime.NumGoroutine()
	hub := NewHub(DefaultLimits())
	profiles := newFakeProfiles()
	pub := profiles.add("alice", true)
	mux := http.NewServeMux()
	NewHandler(hub, profiles, httpapitest.StubSession, httpapitest.NoLimit,
		Config{Keepalive: 10 * time.Millisecond, PublicInterval: 5 * time.Millisecond}).Register(mux)
	srv := httptest.NewServer(mux)

	clients := make([]*client, 0, 30)
	for range 20 {
		u := uuid.New()
		c, _ := open(t, srv, "/api/v1/me/live", u)
		c.next(t)
		hub.MinutesChanged(u, t0, t0.Add(time.Minute))
		clients = append(clients, c)
	}
	for range 10 {
		c, _ := open(t, srv, "/api/v1/users/alice/live", uuid.Nil)
		c.next(t)
		clients = append(clients, c)
	}
	hub.MinutesChanged(pub, t0, t0.Add(time.Minute)) // arms each public check timer
	waitFor(t, "30 subscriptions", func() bool { return hub.Count() == 30 })
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
	srv := newServer(t, hub, newFakeProfiles(), Config{})
	c, _ := open(t, srv, "/api/v1/me/live", uuid.New())
	c.next(t)
	hub.Close()
	c.waitEnd(t)
	if _, code := open(t, srv, "/api/v1/me/live", uuid.New()); code != http.StatusServiceUnavailable {
		t.Fatalf("after Close status = %d, want 503", code)
	}
}
