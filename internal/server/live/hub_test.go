package live

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func must(t *testing.T) func(*Subscription, error) *Subscription {
	return func(s *Subscription, err error) *Subscription {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
}

func TestHubFanOutIsPerUser(t *testing.T) {
	h := NewHub(DefaultLimits())
	a, b := uuid.New(), uuid.New()
	sa1 := must(t)(h.SubscribeOwner(a))
	sa2 := must(t)(h.SubscribePublic(a, "203.0.113.1"))
	sb := must(t)(h.SubscribeOwner(b))

	h.MinutesChanged(a, t0, t0.Add(time.Minute))
	for _, s := range []*Subscription{sa1, sa2} {
		select {
		case <-s.Notify():
		default:
			t.Fatal("subscriber of a not notified")
		}
		if ev, ok := s.Take(); !ok || !ev.From.Equal(t0) {
			t.Fatalf("Take() = %+v, %v", ev, ok)
		}
	}
	select {
	case <-sb.Notify():
		t.Fatal("b notified about a's minutes")
	default:
	}
	if _, ok := sb.Take(); ok {
		t.Fatal("b has a pending event")
	}
}

func TestHubCoalescesForSlowSubscriber(t *testing.T) {
	h := NewHub(DefaultLimits())
	u := uuid.New()
	s := must(t)(h.SubscribeOwner(u))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10_000 {
			at := t0.Add(time.Duration(i%500) * time.Minute)
			h.MinutesChanged(u, at, at.Add(time.Minute))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("MinutesChanged blocked on a subscriber that never reads")
	}
	if n := len(s.Notify()); n != 1 {
		t.Fatalf("%d notifications queued, want 1", n)
	}
	ev, ok := s.Take()
	if !ok || !ev.From.Equal(t0) || !ev.To.Equal(t0.Add(500*time.Minute)) {
		t.Fatalf("coalesced event = %+v, %v", ev, ok)
	}
	if _, ok := s.Take(); ok {
		t.Fatal("event not cleared by Take")
	}
}

func TestHubLimits(t *testing.T) {
	limits := Limits{PerUser: 2, Owner: 3, PerProfile: 2, PerClient: 2, Public: 3}
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	tests := []struct {
		name string
		run  func(t *testing.T, h *Hub)
	}{
		{"per user", func(t *testing.T, h *Hub) {
			must(t)(h.SubscribeOwner(a))
			must(t)(h.SubscribeOwner(a))
			if _, err := h.SubscribeOwner(a); !errors.Is(err, ErrUserLimit) {
				t.Fatalf("third owner stream err = %v", err)
			}
		}},
		{"owner global", func(t *testing.T, h *Hub) {
			must(t)(h.SubscribeOwner(a))
			must(t)(h.SubscribeOwner(b))
			must(t)(h.SubscribeOwner(c))
			if _, err := h.SubscribeOwner(uuid.New()); !errors.Is(err, ErrGlobalLimit) {
				t.Fatalf("owner global err = %v", err)
			}
		}},
		{"per profile", func(t *testing.T, h *Hub) {
			must(t)(h.SubscribePublic(a, "ip1"))
			must(t)(h.SubscribePublic(a, "ip2"))
			if _, err := h.SubscribePublic(a, "ip3"); !errors.Is(err, ErrUserLimit) {
				t.Fatalf("third viewer err = %v", err)
			}
		}},
		{"per client", func(t *testing.T, h *Hub) {
			must(t)(h.SubscribePublic(a, "ip1"))
			must(t)(h.SubscribePublic(b, "ip1"))
			if _, err := h.SubscribePublic(c, "ip1"); !errors.Is(err, ErrClientLimit) {
				t.Fatalf("third stream from one client err = %v", err)
			}
			must(t)(h.SubscribePublic(c, "ip2"))
		}},
		{"public global", func(t *testing.T, h *Hub) {
			must(t)(h.SubscribePublic(a, "ip1"))
			must(t)(h.SubscribePublic(b, "ip2"))
			must(t)(h.SubscribePublic(c, "ip3"))
			if _, err := h.SubscribePublic(uuid.New(), "ip4"); !errors.Is(err, ErrGlobalLimit) {
				t.Fatalf("public global err = %v", err)
			}
		}},
		{"public streams never use owner capacity", func(t *testing.T, h *Hub) {
			for i := range limits.Public {
				must(t)(h.SubscribePublic(uuid.New(), string(rune('a'+i))))
			}
			for _, u := range []uuid.UUID{a, b, c} {
				must(t)(h.SubscribeOwner(u))
			}
		}},
		{"owner streams never use public capacity", func(t *testing.T, h *Hub) {
			for _, u := range []uuid.UUID{a, b, c} {
				must(t)(h.SubscribeOwner(u))
			}
			must(t)(h.SubscribePublic(a, "ip1"))
		}},
		{"close frees the slot and is idempotent", func(t *testing.T, h *Hub) {
			s := must(t)(h.SubscribePublic(a, "ip1"))
			must(t)(h.SubscribePublic(b, "ip1"))
			s.Close()
			s.Close()
			if h.Count() != 1 {
				t.Fatalf("Count() = %d after close, want 1", h.Count())
			}
			must(t)(h.SubscribePublic(c, "ip1"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHub(limits)
			tt.run(t, h)
		})
	}
}

func TestHubCountersReturnToZero(t *testing.T) {
	h := NewHub(DefaultLimits())
	u := uuid.New()
	for range 3 {
		must(t)(h.SubscribeOwner(u))
		must(t)(h.SubscribePublic(u, "ip"))
	}
	h.Close()
	if len(h.subs) != 0 || len(h.ownerByUser) != 0 || len(h.publicByUser) != 0 || len(h.publicByClient) != 0 {
		t.Fatalf("maps not emptied: %d %d %d %d", len(h.subs), len(h.ownerByUser), len(h.publicByUser), len(h.publicByClient))
	}
}

func TestHubProfileMadePrivateEndsPublicStreamsOnly(t *testing.T) {
	h := NewHub(DefaultLimits())
	u := uuid.New()
	own := must(t)(h.SubscribeOwner(u))
	pub := must(t)(h.SubscribePublic(u, "ip"))

	h.ProfileMadePrivate(u)
	select {
	case <-pub.Done():
	default:
		t.Fatal("public stream not ended")
	}
	select {
	case <-own.Done():
		t.Fatal("owner stream ended")
	default:
	}
	if h.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", h.Count())
	}
	pub.Close() // closing after the hub removed it is safe
}

func TestHubClose(t *testing.T) {
	h := NewHub(DefaultLimits())
	s := must(t)(h.SubscribeOwner(uuid.New()))
	h.Close()
	select {
	case <-s.Done():
	default:
		t.Fatal("subscription not ended by Close")
	}
	if _, err := h.SubscribeOwner(uuid.New()); !errors.Is(err, ErrClosed) {
		t.Fatalf("SubscribeOwner after Close err = %v", err)
	}
	if _, err := h.SubscribePublic(uuid.New(), "ip"); !errors.Is(err, ErrClosed) {
		t.Fatalf("SubscribePublic after Close err = %v", err)
	}
	if h.Count() != 0 {
		t.Fatalf("Count() = %d", h.Count())
	}
}

func TestFanoutCallsListenersInOrder(t *testing.T) {
	var calls []string
	f := Fanout{
		listenerFunc(func() { calls = append(calls, "first") }),
		listenerFunc(func() { calls = append(calls, "second") }),
	}
	f.MinutesChanged(uuid.New(), t0, t0.Add(time.Minute))
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("calls = %v", calls)
	}
}

type listenerFunc func()

func (f listenerFunc) MinutesChanged(uuid.UUID, time.Time, time.Time) { f() }
