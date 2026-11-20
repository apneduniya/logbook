package crediting

import (
	"context"
	"slices"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/wire"
)

// futureSlack is how far past the window's end events are cached. Later
// ones (a badly wrong clock) are never credited; heartbeat validation
// already refuses times more than an hour ahead.
const futureSlack = time.Hour

// eventCache holds the events of the sliding window (plus history) in
// memory, so each run reads only what was stored since the previous run:
// about a dozen rows instead of ~2000 on a busy machine. Rows are found by
// id, so late events (an editor's offline queue, the hook spool) are picked
// up whatever their time. It is used by one run at a time.
type eventCache struct {
	seen   localdb.HighWater
	events localdb.Events
}

// refresh reads new events and drops those before from.
func (c *eventCache) refresh(ctx context.Context, s Store, from, to time.Time) error {
	hw, err := s.HighWater(ctx)
	if err != nil {
		return err
	}
	fresh, err := s.EventsAfter(ctx, c.seen, hw, from, to.Add(futureSlack))
	if err != nil {
		return err
	}
	ev := &c.events
	ev.Activity = merge(ev.Activity, fresh.Activity, from, func(e wire.ActivitySample) time.Time { return e.Time })
	ev.Heartbeats = merge(ev.Heartbeats, fresh.Heartbeats, from, wire.Heartbeat.Timestamp)
	ev.AI = merge(ev.AI, fresh.AI, from, func(e wire.AIEvent) time.Time { return e.Time })
	ev.Git = merge(ev.Git, fresh.Git, from, func(e wire.GitEvent) time.Time { return e.Time })
	c.seen = hw
	return nil
}

// merge drops cached events before from and adds fresh ones, keeping the
// slice ordered by time and then by arrival (row id), the order a full read
// of the window returns.
func merge[T any](cached, fresh []T, from time.Time, at func(T) time.Time) []T {
	cached = slices.DeleteFunc(cached, func(e T) bool { return at(e).Before(from) })
	if len(fresh) == 0 {
		return cached
	}
	cached = append(cached, fresh...)
	slices.SortStableFunc(cached, func(a, b T) int { return at(a).Compare(at(b)) })
	return cached
}

// before returns the cached events with time < to, as new slices the
// caller may keep.
func (c *eventCache) before(to time.Time) localdb.Events {
	return localdb.Events{
		Activity:   keepBefore(c.events.Activity, to, func(e wire.ActivitySample) time.Time { return e.Time }),
		Heartbeats: keepBefore(c.events.Heartbeats, to, wire.Heartbeat.Timestamp),
		AI:         keepBefore(c.events.AI, to, func(e wire.AIEvent) time.Time { return e.Time }),
		Git:        keepBefore(c.events.Git, to, func(e wire.GitEvent) time.Time { return e.Time }),
	}
}

func keepBefore[T any](s []T, to time.Time, at func(T) time.Time) []T {
	out := make([]T, 0, len(s))
	for _, e := range s {
		if at(e).Before(to) {
			out = append(out, e)
		}
	}
	return out
}
