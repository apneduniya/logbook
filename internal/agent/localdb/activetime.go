package localdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Active-time types, as Claude Code labels them.
const (
	ActiveUser = "user" // keyboard interaction and reading responses
	ActiveCLI  = "cli"  // tool execution and AI responses
)

// ActiveTimePoint is one stored Claude Code active-time delta: Secs seconds
// of Type during [From, To). It holds no identity attribute; the receiver
// (internal/agent/otlp) drops them before building it.
type ActiveTimePoint struct {
	From, To  time.Time
	SessionID string
	Type      string // ActiveUser or ActiveCLI
	Secs      float64
	// Repository is vcs.repository.name when Claude Code sends it, else "".
	Repository string
	// SeriesStart and Cumulative are set for points converted from a
	// cumulative series: the series' start time and its running total at To.
	SeriesStart time.Time
	Cumulative  *float64
}

// LastCumulative returns the end time and running total of the latest
// stored point of a cumulative series. ok is false when there is none.
func (d *DB) LastCumulative(ctx context.Context, sessionID, typ string, seriesStart time.Time) (to time.Time, total float64, ok bool, err error) {
	var ms int64
	err = d.sql.QueryRowContext(ctx, qActiveLastCumulative, sessionID, typ, seriesMillis(seriesStart)).Scan(&ms, &total)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, 0, false, nil
	}
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("reading active time series: %w", err)
	}
	return time.UnixMilli(ms).UTC(), total, true, nil
}

// InsertActiveTime stores points in one transaction. A point already stored
// for the same session, type and interval (a retried export) is ignored.
// The table is trimmed to the event cap like the raw event tables.
func (d *DB) InsertActiveTime(ctx context.Context, pts []ActiveTimePoint) error {
	if len(pts) == 0 {
		return nil
	}
	for i, p := range pts {
		if err := p.validate(); err != nil {
			return fmt.Errorf("active time point %d: %w", i, err)
		}
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		for _, p := range pts {
			if _, err := tx.ExecContext(ctx, qActiveInsert, p.From.UnixMilli(), p.To.UnixMilli(), p.SessionID,
				p.Type, p.Secs, p.Repository, seriesMillis(p.SeriesStart), p.Cumulative); err != nil {
				return fmt.Errorf("storing active time: %w", err)
			}
		}
		if _, err := trim(ctx, tx, qActiveTrim, d.eventCap); err != nil {
			return err
		}
		return nil
	})
}

// seriesMillis is how a series start is stored: unix ms, or 0 when unset.
func seriesMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// ErrInvalidActiveTime is wrapped when a point can't be stored.
var ErrInvalidActiveTime = errors.New("invalid active time point")

func (p ActiveTimePoint) validate() error {
	switch {
	case p.From.IsZero() || p.To.Before(p.From):
		return fmt.Errorf("%w: bad interval", ErrInvalidActiveTime)
	case p.SessionID == "" || len(p.SessionID) > 256 || len(p.Repository) > 256:
		return fmt.Errorf("%w: bad session or repository", ErrInvalidActiveTime)
	case p.Type != ActiveUser && p.Type != ActiveCLI:
		return fmt.Errorf("%w: type %q", ErrInvalidActiveTime, p.Type)
	case !(p.Secs >= 0): // also rejects NaN
		return fmt.Errorf("%w: negative seconds", ErrInvalidActiveTime)
	}
	return nil
}

// ActiveTimes returns the Claude Code active time overlapping [from, to), as
// the struggle engine takes it: one entry per stored point, with UserSecs or
// CLISecs set by its type, sorted by (From, To, SessionID, type).
// ProjectCwd is the latest working directory Claude Code's hooks reported
// for the session by the point's end ("" when hooks never fired for it);
// OpenTelemetry itself carries no directory.
func (d *DB) ActiveTimes(ctx context.Context, from, to time.Time) ([]struggle.ActiveTime, error) {
	f, t := from.UnixMilli(), to.UnixMilli()
	rows, err := d.sql.QueryContext(ctx, qActiveRange, f, f, t)
	if err != nil {
		return nil, fmt.Errorf("reading active time: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []struggle.ActiveTime
	for rows.Next() {
		var (
			fromMS, toMS int64
			typ          string
			secs         float64
			at           struggle.ActiveTime
		)
		if err := rows.Scan(&fromMS, &toMS, &at.SessionID, &typ, &secs, &at.ProjectCwd); err != nil {
			return nil, fmt.Errorf("scanning active time: %w", err)
		}
		at.From, at.To = time.UnixMilli(fromMS).UTC(), time.UnixMilli(toMS).UTC()
		at.Harness = wire.HarnessClaudeCode
		if typ == ActiveUser {
			at.UserSecs = secs
		} else {
			at.CLISecs = secs
		}
		out = append(out, at)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading active time: %w", err)
	}
	return out, nil
}
