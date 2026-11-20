package live

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Subscription limit errors.
var (
	// ErrUserLimit means the user (or the watched profile) has too many open streams.
	ErrUserLimit = errors.New("too many live streams for this user")
	// ErrGlobalLimit means the server has too many open streams.
	ErrGlobalLimit = errors.New("too many live streams")
	// ErrClosed means the hub is shutting down.
	ErrClosed = errors.New("live hub closed")
)

// Limits caps open subscriptions.
type Limits struct {
	// PerUser caps a user's own (private) streams, e.g. open dashboard tabs.
	PerUser int
	// PerProfile caps public streams watching one profile. It is separate
	// from PerUser so strangers can't use up the owner's own streams.
	PerProfile int
	// Global caps all streams on this server.
	Global int
}

// DefaultLimits are sized for one server process.
func DefaultLimits() Limits {
	return Limits{PerUser: 5, PerProfile: 100, Global: 10_000}
}

// Event is a range of minutes whose rollups changed.
type Event struct {
	From time.Time
	To   time.Time
}

// merge widens e to also cover o.
func (e Event) merge(o Event) Event {
	if o.From.Before(e.From) {
		e.From = o.From
	}
	if o.To.After(e.To) {
		e.To = o.To
	}
	return e
}

// Subscription receives coalesced events for one user.
type Subscription struct {
	hub    *Hub
	userID uuid.UUID
	public bool

	notify chan struct{} // capacity 1: "there is a pending event"
	done   chan struct{} // closed when the subscription ends
	once   sync.Once

	mu      sync.Mutex
	pending Event
	has     bool
}

// Notify fires when at least one event is pending; call Take to read it.
func (s *Subscription) Notify() <-chan struct{} { return s.notify }

// Done is closed when the hub ends the subscription (profile made private,
// shutdown) or after Close.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Take returns and clears the pending event, merged from every event since
// the last Take.
func (s *Subscription) Take() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, ok := s.pending, s.has
	s.pending, s.has = Event{}, false
	return ev, ok
}

// Close unsubscribes. It is safe to call more than once.
func (s *Subscription) Close() { s.hub.remove(s) }

func (s *Subscription) offer(ev Event) {
	s.mu.Lock()
	if s.has {
		s.pending = s.pending.merge(ev)
	} else {
		s.pending, s.has = ev, true
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default: // a notification is already pending; the merge above covers it
	}
}

// Hub fans out minute changes to subscribers. It starts no goroutines.
type Hub struct {
	limits Limits

	mu       sync.Mutex
	subs     map[uuid.UUID]map[*Subscription]struct{}
	private  map[uuid.UUID]int
	public   map[uuid.UUID]int
	total    int
	isClosed bool
}

// NewHub returns a Hub enforcing limits.
func NewHub(limits Limits) *Hub {
	return &Hub{
		limits:  limits,
		subs:    map[uuid.UUID]map[*Subscription]struct{}{},
		private: map[uuid.UUID]int{},
		public:  map[uuid.UUID]int{},
	}
}

// Subscribe opens a subscription to userID's changes. public marks a stream
// opened by a profile viewer rather than the user. Callers must Close it.
func (h *Hub) Subscribe(userID uuid.UUID, public bool) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.isClosed:
		return nil, ErrClosed
	case h.total >= h.limits.Global:
		return nil, ErrGlobalLimit
	case public && h.public[userID] >= h.limits.PerProfile:
		return nil, ErrUserLimit
	case !public && h.private[userID] >= h.limits.PerUser:
		return nil, ErrUserLimit
	}
	s := &Subscription{
		hub: h, userID: userID, public: public,
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if h.subs[userID] == nil {
		h.subs[userID] = map[*Subscription]struct{}{}
	}
	h.subs[userID][s] = struct{}{}
	h.counter(public)[userID]++
	h.total++
	return s, nil
}

// MinutesChanged notifies userID's subscribers that minutes in [from, to)
// changed. It never blocks on subscribers. Ingest calls it after commit.
func (h *Hub) MinutesChanged(userID uuid.UUID, from, to time.Time) {
	ev := Event{From: from.UTC(), To: to.UTC()}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[userID] {
		s.offer(ev)
	}
}

// ProfileMadePrivate ends every public stream watching userID.
func (h *Hub) ProfileMadePrivate(userID uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[userID] {
		if s.public {
			h.removeLocked(s)
		}
	}
}

// Close ends every subscription and refuses new ones. Register it with
// http.Server.RegisterOnShutdown so open streams don't block shutdown.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.isClosed = true
	for _, set := range h.subs {
		for s := range set {
			h.removeLocked(s)
		}
	}
}

// Count returns the number of open subscriptions.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

func (h *Hub) remove(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(s)
}

func (h *Hub) removeLocked(s *Subscription) {
	set := h.subs[s.userID]
	if _, ok := set[s]; ok {
		delete(set, s)
		if len(set) == 0 {
			delete(h.subs, s.userID)
		}
		c := h.counter(s.public)
		if c[s.userID]--; c[s.userID] <= 0 {
			delete(c, s.userID)
		}
		h.total--
	}
	s.once.Do(func() { close(s.done) })
}

func (h *Hub) counter(public bool) map[uuid.UUID]int {
	if public {
		return h.public
	}
	return h.private
}
