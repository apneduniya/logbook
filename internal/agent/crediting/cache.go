package crediting

import (
	"context"
	"slices"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/wire"
)

// futureSlack is how far past the window's end events are cached. Later
// ones (a badly wrong clock) are never credited; heartbeat validation
// already refuses times more than an hour ahead.
const futureSlack = time.Hour

// eventCache holds the events of the sliding window (plus history) in
// memory, so each run reads only what was stored since the previous run:
// about a dozen rows instead of ~2000 on a busy machine. It is used by one
// run at a time.
//
// Its invariant: events holds exactly the rows with id <= seen and
// from <= time < to, in the order a read of that range returns (time, then
// row id). Each refresh keeps it, so the cache always gives what a cold
// read would:
//
//   - rows stored since the last refresh are found by id (> seen), whatever
//     their time, so late events (an editor's offline queue, the hook
//     spool) are picked up;
//   - when the range's end moves forward, older rows that now fall inside
//     it are read by time (ids <= seen in the new part);
//   - when the range moves backward, or jumps past its old end (the clock
//     was changed), or the tables' ids went back, the cache is refilled
//     from scratch: rows it dropped, or never read, may be needed again;
//   - when rows were deleted (pruned, or trimmed by the row cap), which the
//     ids can't show, the deletion generation has moved and the cache is
//     refilled too, so it never keeps a row the table no longer has.
type eventCache struct {
	filled   bool
	seen     localdb.HighWater
	gen      int64
	from, to time.Time
	events   localdb.Events
}

// cacheStore is what the cache reads. *localdb.DB satisfies it.
type cacheStore interface {
	EventsBetween(ctx context.Context, from, to time.Time, upto *localdb.HighWater) (localdb.Events, error)
	EventsAfter(ctx context.Context, after, upto localdb.HighWater, from, to time.Time) (localdb.Events, error)
	HighWater(ctx context.Context) (localdb.HighWater, error)
	EventGeneration(ctx context.Context) (int64, error)
}

// refresh makes the cache hold the events with from <= time <
// to+futureSlack, as of the tables' current high-water ids.
func (c *eventCache) refresh(ctx context.Context, s cacheStore, from, to time.Time) error {
	to = to.Add(futureSlack)
	// The generation is read first: a deletion after it is seen by the next
	// refresh, never missed.
	gen, err := s.EventGeneration(ctx)
	if err != nil {
		return err
	}
	hw, err := s.HighWater(ctx)
	if err != nil {
		return err
	}
	if !c.filled || gen != c.gen || from.Before(c.from) || from.After(c.to) || to.Before(c.to) || below(hw, c.seen) {
		// Read the range by time: walking ids from zero would scan every
		// row of a 90-day database.
		ev, err := s.EventsBetween(ctx, from, to, &hw)
		if err != nil {
			return err
		}
		*c = eventCache{filled: true, seen: hw, gen: gen, from: from, to: to, events: ev}
		return nil
	}
	// Rows already seen whose time has just come into range, then rows
	// stored since. Within equal times this is id order: the first are
	// all <= seen, the second all > seen.
	var grown localdb.Events
	if to.After(c.to) {
		if grown, err = s.EventsBetween(ctx, c.to, to, &c.seen); err != nil {
			return err
		}
	}
	fresh, err := s.EventsAfter(ctx, c.seen, hw, from, to)
	if err != nil {
		return err
	}
	ev := &c.events
	ev.Activity = merge(ev.Activity, from, activityTime, grown.Activity, fresh.Activity)
	ev.Heartbeats = merge(ev.Heartbeats, from, heartbeatTime, grown.Heartbeats, fresh.Heartbeats)
	ev.AI = merge(ev.AI, from, aiTime, grown.AI, fresh.AI)
	ev.Git = merge(ev.Git, from, gitTime, grown.Git, fresh.Git)
	c.seen, c.from, c.to = hw, from, to
	return nil
}

// below reports whether any of a's ids is below b's: rows were removed
// from the end of a table, so ids no longer only grow.
func below(a, b localdb.HighWater) bool {
	return a.Heartbeats < b.Heartbeats || a.AI < b.AI || a.Activity < b.Activity || a.Git < b.Git
}

// merge drops cached events before from and appends the added slices in
// order, keeping the result sorted by time and, within equal times, in the
// order given (which is row-id order).
func merge[T any](cached []T, from time.Time, at func(T) time.Time, added ...[]T) []T {
	cached = slices.DeleteFunc(cached, func(e T) bool { return at(e).Before(from) })
	n := len(cached)
	for _, a := range added {
		cached = append(cached, a...)
	}
	if len(cached) == n {
		return cached
	}
	slices.SortStableFunc(cached, func(a, b T) int { return at(a).Compare(at(b)) })
	return cached
}

// Event times as stored and ordered by the local database: unix
// milliseconds. A heartbeat's payload may carry finer times, which the
// database orders by row id within the millisecond.
func activityTime(e evidence.Received[wire.ActivitySample]) time.Time { return e.Event.Time }
func aiTime(e evidence.Received[wire.AIEvent]) time.Time              { return e.Event.Time }
func gitTime(e evidence.Received[wire.GitEvent]) time.Time            { return e.Event.Time }
func heartbeatTime(h evidence.Received[wire.Heartbeat]) time.Time {
	return time.UnixMilli(h.Event.Timestamp().UnixMilli()).UTC()
}

// before returns the cached events with time < to, as new slices the
// caller may keep.
func (c *eventCache) before(to time.Time) localdb.Events {
	return within(c.events, time.Time{}, to)
}

// within returns the events of ev with from <= time < to, as new slices.
func within(ev localdb.Events, from, to time.Time) localdb.Events {
	return localdb.Events{
		Activity:   keepBetween(ev.Activity, from, to, activityTime),
		Heartbeats: keepBetween(ev.Heartbeats, from, to, heartbeatTime),
		AI:         keepBetween(ev.AI, from, to, aiTime),
		Git:        keepBetween(ev.Git, from, to, gitTime),
	}
}

func keepBetween[T any](s []T, from, to time.Time, at func(T) time.Time) []T {
	out := make([]T, 0, len(s))
	for _, e := range s {
		if t := at(e); !t.Before(from) && t.Before(to) {
			out = append(out, e)
		}
	}
	return out
}
