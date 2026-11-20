package stats

import (
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Streak cache sizing. A streak needs every qualifying day of the user's
// history, so the full list is loaded once per user (and time zone) and then
// patched: MinutesChanged marks the changed range dirty and the next read
// re-queries only those local days. streakCacheTTL forces a full reload now
// and then as a safety net for writes that bypass MinutesChanged.
const (
	streakCacheTTL        = 15 * time.Minute
	streakCacheMaxEntries = 10_000
)

// streakCache holds each user's qualifying local days.
type streakCache struct {
	ttl time.Duration
	max int

	mu    sync.Mutex
	users map[uuid.UUID]*streakEntry
}

// streakEntry is one user's cached days. refresh serialises the queries
// that update it; every other field is guarded by streakCache.mu. Lock
// order is always refresh, then streakCache.mu.
type streakEntry struct {
	refresh sync.Mutex

	tz       string
	days     []Date // sorted, no duplicates
	loadedAt time.Time
	loaded   bool

	// dirty is the instant range changed since the last refresh.
	dirty      bool
	dirtyRange span
}

// span is a range of instants whose minutes changed, [From, To).
type span struct{ From, To time.Time }

func newStreakCache() *streakCache {
	return &streakCache{ttl: streakCacheTTL, max: streakCacheMaxEntries, users: map[uuid.UUID]*streakEntry{}}
}

// refreshPlan says what a read must query before the cache is current.
type refreshPlan struct {
	full    bool
	partial bool
	rng     span
}

// entry returns userID's entry, creating it (and evicting an arbitrary one
// when full; losing it only costs a reload).
func (c *streakCache) entry(userID uuid.UUID) *streakEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.users[userID]; ok {
		return e
	}
	for k := range c.users {
		if len(c.users) < c.max {
			break
		}
		delete(c.users, k)
	}
	e := &streakEntry{}
	c.users[userID] = e
	return e
}

// plan decides what e needs and clears the dirty range it hands out, so
// changes arriving while the caller queries are kept for the next read.
// The caller holds e.refresh.
func (c *streakCache) plan(e *streakEntry, tz string, now time.Time) refreshPlan {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !e.loaded || e.tz != tz || now.Sub(e.loadedAt) >= c.ttl {
		e.loaded, e.dirty = false, false
		return refreshPlan{full: true}
	}
	if e.dirty {
		e.dirty = false
		return refreshPlan{partial: true, rng: e.dirtyRange}
	}
	return refreshPlan{}
}

// storeFull replaces e's days after a full load.
func (c *streakCache) storeFull(e *streakEntry, tz string, days []Date, loadedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.tz, e.days, e.loadedAt, e.loaded = tz, days, loadedAt, true
}

// storeRange replaces e's days in [first, last] with days.
func (c *streakCache) storeRange(e *streakEntry, first, last Date, days []Date) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := slices.DeleteFunc(slices.Clone(e.days), func(d Date) bool {
		return !d.Before(first) && !last.Before(d)
	})
	kept = append(kept, days...)
	slices.SortFunc(kept, func(a, b Date) int { return a.t.Compare(b.t) })
	e.days = slices.CompactFunc(kept, Date.Equal)
}

// markDirty re-marks rng after a failed partial refresh.
func (c *streakCache) markDirty(e *streakEntry, rng span) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.markDirtyLocked(rng)
}

// snapshot returns a copy of e's days.
func (c *streakCache) snapshot(e *streakEntry) []Date {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(e.days)
}

// invalidate records that userID's minutes in [from, to) changed. It never
// queries, so ingest's call stays cheap.
func (c *streakCache) invalidate(userID uuid.UUID, from, to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.users[userID]; ok {
		e.markDirtyLocked(span{From: from, To: to})
	}
}

func (e *streakEntry) markDirtyLocked(rng span) {
	if !e.dirty {
		e.dirty, e.dirtyRange = true, rng
		return
	}
	if rng.From.Before(e.dirtyRange.From) {
		e.dirtyRange.From = rng.From
	}
	if rng.To.After(e.dirtyRange.To) {
		e.dirtyRange.To = rng.To
	}
}
