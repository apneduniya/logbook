package live

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func TestHubFanOutIsPerUser(t *testing.T) {
	h := NewHub(DefaultLimits())
	a, b := uuid.New(), uuid.New()
	sa1, _ := h.Subscribe(a, false)
	sa2, _ := h.Subscribe(a, true)
	sb, _ := h.Subscribe(b, false)
	defer sa1.Close()
	defer sa2.Close()
	defer sb.Close()

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
	s, _ := h.Subscribe(u, false)
	defer s.Close()

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
	h := NewHub(Limits{PerUser: 2, PerProfile: 1, Global: 4})
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	must := func(s *Subscription, err error) *Subscription {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a1 := must(h.Subscribe(a, false))
	must(h.Subscribe(a, false))
	if _, err := h.Subscribe(a, false); !errors.Is(err, ErrUserLimit) {
		t.Fatalf("third private stream err = %v", err)
	}
	// Public viewers have their own cap, so they can't lock the owner out and vice versa.
	must(h.Subscribe(a, true))
	if _, err := h.Subscribe(a, true); !errors.Is(err, ErrUserLimit) {
		t.Fatalf("second public stream err = %v", err)
	}
	must(h.Subscribe(b, false))
	if _, err := h.Subscribe(c, false); !errors.Is(err, ErrGlobalLimit) {
		t.Fatalf("global limit err = %v", err)
	}
	a1.Close()
	a1.Close() // idempotent
	if h.Count() != 3 {
		t.Fatalf("Count() = %d after close, want 3", h.Count())
	}
	must(h.Subscribe(c, false))
}

func TestHubProfileMadePrivateEndsPublicStreamsOnly(t *testing.T) {
	h := NewHub(DefaultLimits())
	u := uuid.New()
	own, _ := h.Subscribe(u, false)
	pub, _ := h.Subscribe(u, true)
	defer own.Close()

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
	s, _ := h.Subscribe(uuid.New(), false)
	h.Close()
	select {
	case <-s.Done():
	default:
		t.Fatal("subscription not ended by Close")
	}
	if _, err := h.Subscribe(uuid.New(), false); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close err = %v", err)
	}
	if h.Count() != 0 {
		t.Fatalf("Count() = %d", h.Count())
	}
}
