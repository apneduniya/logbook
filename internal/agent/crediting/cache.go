package crediting

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
)

// futureSlack is how far past the window's end events are cached. Later
// ones (a badly wrong clock) are never credited; heartbeat validation
// already refuses times more than an hour ahead.
const futureSlack = time.Hour

// eventCache holds the events of the sliding window (plus history) in
// memory, so each run reads only what was logged since the previous run:
// about a dozen rows instead of ~2000 on a busy machine. It is used by one
// run at a time.
//
// Its invariant: events holds exactly the logged rows with seq <= seen and
// from <= time < to, ordered by time, then sequence number. Each refresh
// runs inside the run's snapshot and moves seen to the snapshot's H, so
// the cache always gives what a cold read of the snapshot would:
//
//   - rows logged since the last refresh are read by sequence number
//     (> seen), whatever their time, so late events (an editor's offline
//     queue, the hook spool) are picked up;
//   - when the range's end moves forward, older rows that now fall inside
//     it are read by time (seq <= seen in the new part);
//   - when the range moves backward, or jumps past its old end (the clock
//     was changed), the cache is refilled from scratch: rows it dropped, or
//     never read, may be needed again;
//   - when rows were deleted (pruned, or trimmed by the row cap), which
//     sequence numbers can't show, the deletion generation has moved and
//     the cache is refilled too. The generation is read in the same
//     snapshot as the rows, so no deletion can slip between them.
type eventCache struct {
	filled   bool
	seen     int64
	gen      int64
	from, to time.Time
	events   localdb.Events
}

// refresh makes the cache hold the events of snapshot s with from <= time <
// to+futureSlack.
func (c *eventCache) refresh(ctx context.Context, s *localdb.Snapshot, from, to time.Time) error {
	to = to.Add(futureSlack)
	if !c.filled || s.Generation != c.gen || s.H < c.seen || from.Before(c.from) || from.After(c.to) || to.Before(c.to) {
		// Read the range by time: walking sequence numbers from zero would
		// scan every row of a 90-day database.
		ev, err := s.Events(ctx, from, to, 0, s.H)
		if err != nil {
			return err
		}
		*c = eventCache{filled: true, seen: s.H, gen: s.Generation, from: from, to: to, events: ev}
		return nil
	}
	// Rows already seen whose time has just come into range, then rows
	// logged since.
	var grown localdb.Events
	if to.After(c.to) {
		var err error
		if grown, err = s.Events(ctx, c.to, to, 0, c.seen); err != nil {
			return err
		}
	}
	var fresh localdb.Events
	if s.H > c.seen {
		var err error
		if fresh, err = s.Events(ctx, from, to, c.seen, s.H); err != nil {
			return err
		}
	}
	ev := &c.events
	ev.Activity = merge(ev.Activity, from, activityTime, grown.Activity, fresh.Activity)
	ev.Heartbeats = merge(ev.Heartbeats, from, heartbeatTime, grown.Heartbeats, fresh.Heartbeats)
	ev.AI = merge(ev.AI, from, aiTime, grown.AI, fresh.AI)
	ev.Git = merge(ev.Git, from, gitTime, grown.Git, fresh.Git)
	c.seen, c.from, c.to = s.H, from, to
	return nil
}

// merge drops cached events before from, appends the added slices and
// keeps the result ordered by time, then sequence number.
func merge[T any](cached []localdb.Logged[T], from time.Time, at func(T) time.Time, added ...[]localdb.Logged[T]) []localdb.Logged[T] {
	cached = slices.DeleteFunc(cached, func(e localdb.Logged[T]) bool { return at(e.Event).Before(from) })
	n := len(cached)
	for _, a := range added {
		cached = append(cached, a...)
	}
	if len(cached) == n {
		return cached
	}
	slices.SortFunc(cached, func(a, b localdb.Logged[T]) int {
		if c := at(a.Event).Compare(at(b.Event)); c != 0 {
			return c
		}
		return cmp.Compare(a.Seq, b.Seq)
	})
	return cached
}

// before returns the cached events with time < to, as new slices the
// caller may keep.
func (c *eventCache) before(to time.Time) localdb.Events {
	return localdb.Events{
		Activity:   keepBefore(c.events.Activity, to, activityTime),
		Heartbeats: keepBefore(c.events.Heartbeats, to, heartbeatTime),
		AI:         keepBefore(c.events.AI, to, aiTime),
		Git:        keepBefore(c.events.Git, to, gitTime),
	}
}

func keepBefore[T any](s []localdb.Logged[T], to time.Time, at func(T) time.Time) []localdb.Logged[T] {
	out := make([]localdb.Logged[T], 0, len(s))
	for _, e := range s {
		if at(e.Event).Before(to) {
			out = append(out, e)
		}
	}
	return out
}
