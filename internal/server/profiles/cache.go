package profiles

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Cache sizing. A public profile's aggregates scan the user's whole minute
// history, so they are computed at most once per user per cacheTTL unless a
// live stream asks for fresher data (see Service.PublicVersion).
const (
	cacheTTL        = 30 * time.Second
	cacheMaxEntries = 10_000
)

// aggregates is the cached, time-consuming part of a public profile.
type aggregates struct {
	public     Public // Totals, Projects and Languages only
	version    string
	computedAt time.Time // when the load started
}

// aggCache holds per-user aggregates. Concurrent requests for the same user
// share one load, so a burst of viewers costs one query.
type aggCache struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[uuid.UUID]*aggEntry
}

type aggEntry struct {
	mu  sync.Mutex // held while loading, so loads for one user are serialised
	val *aggregates
}

func newAggCache(now func() time.Time) *aggCache {
	return &aggCache{ttl: cacheTTL, max: cacheMaxEntries, now: now, entries: map[uuid.UUID]*aggEntry{}}
}

// get returns userID's aggregates, loading them when there is no entry, it
// is older than the TTL, or it was computed before freshAfter.
func (c *aggCache) get(ctx context.Context, userID uuid.UUID, freshAfter time.Time,
	load func(context.Context) (aggregates, error),
) (aggregates, error) {
	e := c.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := c.now()
	if v := e.val; v != nil && !v.computedAt.Before(freshAfter) && now.Sub(v.computedAt) < c.ttl {
		return *v, nil
	}
	v, err := load(ctx)
	if err != nil {
		return aggregates{}, err
	}
	v.computedAt = now
	e.val = &v
	return v, nil
}

// invalidate drops userID's entry so the next read reloads it.
func (c *aggCache) invalidate(userID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, userID)
}

func (c *aggCache) entry(userID uuid.UUID) *aggEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[userID]; ok {
		return e
	}
	// Full: drop arbitrary entries. Losing one only costs a reload.
	for k := range c.entries {
		if len(c.entries) < c.max {
			break
		}
		delete(c.entries, k)
	}
	e := &aggEntry{}
	c.entries[userID] = e
	return e
}
