package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

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

// Events are the raw local events of a time range, each slice oldest first.
// Times are UTC with millisecond precision (how they are stored);
// heartbeats are exactly as accepted.
type Events struct {
	Activity   []wire.ActivitySample
	Heartbeats []wire.Heartbeat
	AI         []wire.AIEvent
	Git        []wire.GitEvent
}

// EventsBetween returns every raw event with from <= time < to. When upto is
// non-nil, only rows at or below its ids are read. Heartbeats that can no
// longer be decoded are skipped (they can't have come from Enqueue).
func (d *DB) EventsBetween(ctx context.Context, from, to time.Time, upto *HighWater) (Events, error) {
	lim := HighWater{math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64}
	if upto != nil {
		lim = *upto
	}
	f, t := from.UnixMilli(), to.UnixMilli()
	var ev Events
	err := d.query(ctx, qHeartbeatsRange, []any{f, t, lim.Heartbeats}, func(r *sql.Rows) error {
		var payload []byte
		if err := r.Scan(&payload); err != nil {
			return err
		}
		var h wire.Heartbeat
		if json.Unmarshal(payload, &h) == nil {
			ev.Heartbeats = append(ev.Heartbeats, h)
		}
		return nil
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading heartbeats: %w", err)
	}
	err = d.query(ctx, qAIRange, []any{f, t, lim.AI}, func(r *sql.Rows) error {
		var (
			e             wire.AIEvent
			at            int64
			harness, kind string
		)
		if err := r.Scan(&at, &harness, &kind, &e.SessionID, &e.Project, &e.Cwd, &e.Model, &e.Tool,
			&e.PromptChars, &e.Decision); err != nil {
			return err
		}
		e.Time, e.Harness, e.Kind = time.UnixMilli(at).UTC(), wire.Harness(harness), wire.AIEventKind(kind)
		ev.AI = append(ev.AI, e)
		return nil
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading ai events: %w", err)
	}
	err = d.query(ctx, qActivityRange, []any{f, t, lim.Activity}, func(r *sql.Rows) error {
		var (
			s  wire.ActivitySample
			at int64
		)
		if err := r.Scan(&at, &s.IdleSeconds, &s.FocusApp, &s.FocusKind); err != nil {
			return err
		}
		s.Time = time.UnixMilli(at).UTC()
		ev.Activity = append(ev.Activity, s)
		return nil
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading activity samples: %w", err)
	}
	err = d.query(ctx, qGitRange, []any{f, t, lim.Git}, func(r *sql.Rows) error {
		var (
			g  wire.GitEvent
			at int64
		)
		if err := r.Scan(&at, &g.Project, &g.Kind, &g.CommitHash, &g.FilesChanged, &g.Insertions, &g.Deletions); err != nil {
			return err
		}
		g.Time = time.UnixMilli(at).UTC()
		ev.Git = append(ev.Git, g)
		return nil
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading git events: %w", err)
	}
	return ev, nil
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
