package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/wire"
)

// HighWater is the highest row id of each raw event table at one moment.
// Reading up to a HighWater reproduces exactly the events that existed
// then, however many late events arrived since.
type HighWater struct {
	Heartbeats, AI, Activity, Git int64
}

// HighWater returns the current highest row id of each event table.
func (d *DB) HighWater(ctx context.Context) (HighWater, error) {
	var hw HighWater
	if err := d.sql.QueryRowContext(ctx, qHighWater).Scan(&hw.Heartbeats, &hw.AI, &hw.Activity, &hw.Git); err != nil {
		return HighWater{}, fmt.Errorf("reading event high-water marks: %w", err)
	}
	return hw, nil
}

// Events are the raw local events of a time range, each slice oldest first
// (by event time, then row id), each with the time it was stored (its
// arrival, ADR 0016). Times are UTC with millisecond precision (how they
// are stored); heartbeats are exactly as accepted.
type Events struct {
	Activity   []evidence.Received[wire.ActivitySample]
	Heartbeats []evidence.Received[wire.Heartbeat]
	AI         []evidence.Received[wire.AIEvent]
	Git        []evidence.Received[wire.GitEvent]
}

// EventsBetween returns every raw event with from <= time < to. When upto is
// non-nil, only rows at or below its ids are read. Heartbeats that can no
// longer be decoded are skipped (they can't have come from Enqueue).
func (d *DB) EventsBetween(ctx context.Context, from, to time.Time, upto *HighWater) (Events, error) {
	lim := HighWater{math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64}
	if upto != nil {
		lim = *upto
	}
	return d.readEvents(ctx, byTime(), from.UnixMilli(), to.UnixMilli(), HighWater{}, lim)
}

// eventQueries is one read query per event table.
type eventQueries struct{ heartbeats, ai, activity, gitEvents string }

// byTime walks the time index: best for a whole range.
func byTime() eventQueries {
	return eventQueries{qHeartbeatsRange, qAIRange, qActivityRange, qGitRange}
}

// byID walks the id range: best for the few rows added since a poll.
func byID() eventQueries {
	return eventQueries{qHeartbeatsAfter, qAIAfter, qActivityAfter, qGitAfter}
}

// EventsAfter returns the events stored since after, up to and including
// upto, with from <= time < to. Polling it with the previous call's upto
// reads each event once, so a caller can keep a sliding window in memory
// instead of re-reading it.
func (d *DB) EventsAfter(ctx context.Context, after, upto HighWater, from, to time.Time) (Events, error) {
	return d.readEvents(ctx, byID(), from.UnixMilli(), to.UnixMilli(), after, upto)
}

// readEvents reads events with f <= time (ms) < t and after < id <= upto.
func (d *DB) readEvents(ctx context.Context, q eventQueries, f, t int64, after, upto HighWater) (Events, error) {
	var ev Events
	err := d.query(ctx, q.heartbeats, []any{f, t, after.Heartbeats, upto.Heartbeats}, func(r *sql.Rows) error {
		var (
			payload []byte
			arrived int64
		)
		if err := r.Scan(&payload, &arrived); err != nil {
			return err
		}
		var h wire.Heartbeat
		if json.Unmarshal(payload, &h) == nil {
			ev.Heartbeats = append(ev.Heartbeats, received(h, arrived))
		}
		return nil
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading heartbeats: %w", err)
	}
	err = d.query(ctx, q.ai, []any{f, t, after.AI, upto.AI}, func(r *sql.Rows) error {
		var (
			e                 wire.AIEvent
			at, arrived       int64
			harness, kindName string
		)
		if err := r.Scan(&at, &harness, &kindName, &e.SessionID, &e.Project, &e.Cwd, &e.Model, &e.Tool,
			&e.PromptChars, &e.Decision, &e.PromptHash, &arrived); err != nil {
			return err
		}
		e.Time, e.Harness, e.Kind = time.UnixMilli(at).UTC(), wire.Harness(harness), wire.AIEventKind(kindName)
		ev.AI = append(ev.AI, received(e, arrived))
		return nil
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading ai events: %w", err)
	}
	err = d.query(ctx, q.activity, []any{f, t, after.Activity, upto.Activity}, func(r *sql.Rows) error {
		var (
			s           wire.ActivitySample
			at, arrived int64
		)
		if err := r.Scan(&at, &s.IdleSeconds, &s.FocusApp, &s.FocusKind, &arrived); err != nil {
			return err
		}
		s.Time = time.UnixMilli(at).UTC()
		ev.Activity = append(ev.Activity, received(s, arrived))
		return nil
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading activity samples: %w", err)
	}
	err = d.query(ctx, q.gitEvents, []any{f, t, after.Git, upto.Git}, func(r *sql.Rows) error {
		var (
			g           wire.GitEvent
			at, arrived int64
		)
		if err := r.Scan(&at, &g.Project, &g.Kind, &g.CommitHash, &g.FilesChanged, &g.Insertions, &g.Deletions,
			&arrived); err != nil {
			return err
		}
		g.Time = time.UnixMilli(at).UTC()
		ev.Git = append(ev.Git, received(g, arrived))
		return nil
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading git events: %w", err)
	}
	return ev, nil
}

// received pairs an event with its stored arrival (unix ms).
func received[T any](e T, arrivedMS int64) evidence.Received[T] {
	return evidence.Received[T]{Event: e, Arrived: time.UnixMilli(arrivedMS).UTC()}
}

// query runs q and calls scan for every row.
func (d *DB) query(ctx context.Context, q string, args []any, scan func(*sql.Rows) error) error {
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
