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
	// ErrClientLimit means one client (IP) has too many public streams open.
	ErrClientLimit = errors.New("too many live streams from this client")
	// ErrGlobalLimit means the server has too many open streams of that kind.
	ErrGlobalLimit = errors.New("too many live streams")
	// ErrClosed means the hub is shutting down.
	ErrClosed = errors.New("live hub closed")
)

// Limits caps open subscriptions. Owner streams (a user's own dashboard) and
// public streams (anonymous profile viewers) have separate budgets, so
// strangers can never use up the capacity signed-in users need.
type Limits struct {
	// PerUser caps a user's own streams, e.g. open dashboard tabs.
	PerUser int
	// Owner caps all owner streams on this server.
	Owner int

	// PerProfile caps public streams watching one profile.
	PerProfile int
	// PerClient caps public streams opened by one client (IP address).
	PerClient int
	// Public caps all public streams on this server.
	Public int
}

// DefaultLimits are sized for one server process. PerClient allows a
// shared NAT (a school, a hackathon venue) a few viewers per person.
func DefaultLimits() Limits {
	return Limits{PerUser: 5, Owner: 10_000, PerProfile: 100, PerClient: 20, Public: 2_000}
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
	client string // public streams only

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

	mu             sync.Mutex
	subs           map[uuid.UUID]map[*Subscription]struct{}
	ownerByUser    map[uuid.UUID]int
	publicByUser   map[uuid.UUID]int
	publicByClient map[string]int
	owners         int
	publics        int
	isClosed       bool
}

// NewHub returns a Hub enforcing limits.
func NewHub(limits Limits) *Hub {
	return &Hub{
		limits:         limits,
		subs:           map[uuid.UUID]map[*Subscription]struct{}{},
		ownerByUser:    map[uuid.UUID]int{},
		publicByUser:   map[uuid.UUID]int{},
		publicByClient: map[string]int{},
	}
}

// SubscribeOwner opens a subscription to the user's own changes. Callers
// must Close it.
func (h *Hub) SubscribeOwner(userID uuid.UUID) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.isClosed:
		return nil, ErrClosed
	case h.owners >= h.limits.Owner:
		return nil, ErrGlobalLimit
	case h.ownerByUser[userID] >= h.limits.PerUser:
		return nil, ErrUserLimit
	}
	h.ownerByUser[userID]++
	h.owners++
	return h.addLocked(userID, false, ""), nil
}

// SubscribePublic opens a profile viewer's subscription to userID's changes.
// client identifies the viewer (normally its IP) for the per-client cap.
// Callers must Close it.
func (h *Hub) SubscribePublic(userID uuid.UUID, client string) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.isClosed:
		return nil, ErrClosed
	case h.publics >= h.limits.Public:
		return nil, ErrGlobalLimit
	case h.publicByClient[client] >= h.limits.PerClient:
		return nil, ErrClientLimit
	case h.publicByUser[userID] >= h.limits.PerProfile:
		return nil, ErrUserLimit
	}
	h.publicByUser[userID]++
	h.publicByClient[client]++
	h.publics++
	return h.addLocked(userID, true, client), nil
}

func (h *Hub) addLocked(userID uuid.UUID, public bool, client string) *Subscription {
	s := &Subscription{
		hub: h, userID: userID, public: public, client: client,
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if h.subs[userID] == nil {
		h.subs[userID] = map[*Subscription]struct{}{}
	}
	h.subs[userID][s] = struct{}{}
	return s
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

// Count returns the number of open subscriptions of both kinds.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.owners + h.publics
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
		if s.public {
			decrement(h.publicByUser, s.userID)
			decrement(h.publicByClient, s.client)
			h.publics--
		} else {
			decrement(h.ownerByUser, s.userID)
			h.owners--
		}
	}
	s.once.Do(func() { close(s.done) })
}

// decrement lowers m[k] and deletes it at zero so maps don't grow forever.
func decrement[K comparable](m map[K]int, k K) {
	if m[k]--; m[k] <= 0 {
		delete(m, k)
	}
}
