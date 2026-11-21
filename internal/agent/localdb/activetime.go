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

// InsertActiveTime stores points in one transaction, each taking the next
// sequence number of the event log (ADR 0017). A point already stored for
// the same session, type and interval (a retried export) is ignored and
// takes none. Over the event cap, only points the evidence chain sealed are
// trimmed; active time is always kept.
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
		now := d.now()
		w, err := openLog(ctx, tx)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, qActiveInsert)
		if err != nil {
			return fmt.Errorf("preparing: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		for _, p := range pts {
			if err := w.insert(ctx, stmt, p.From.UnixMilli(), p.To.UnixMilli(), p.SessionID,
				p.Type, p.Secs, p.Repository, seriesMillis(p.SeriesStart), p.Cumulative); err != nil {
				return fmt.Errorf("storing active time: %w", err)
			}
		}
		if err := w.close(ctx, tx, now); err != nil {
			return err
		}
		sealed, err := sealedSeq(ctx, tx)
		if err != nil {
			return err
		}
		_, err = trimSealed(ctx, tx, qActiveTrim, d.eventCap, sealed)
		return err
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

// UptimeMarkEvery is how often the daemon calls MarkReceiverUp while its
// OTLP receiver runs.
const UptimeMarkEvery = 30 * time.Second

// Coverage margins: a receiver run that started at a and was last marked at
// b counts as covering [a-coverBefore, b+coverAfter].
const (
	// coverBefore: an export carries up to one export interval (60 s) of
	// active time from before it arrives, and the exporter retries a
	// failed export for a while.
	coverBefore = 2 * time.Minute
	// coverAfter: a running receiver's latest mark lags by up to one mark
	// interval.
	coverAfter = 2 * UptimeMarkEvery
)

// MarkReceiverUp records that the OTLP receiver run that started at started
// was up at at.
func (d *DB) MarkReceiverUp(ctx context.Context, started, at time.Time) error {
	if _, err := d.sql.ExecContext(ctx, qUptimeMark, started.UnixMilli(), at.UnixMilli()); err != nil {
		return fmt.Errorf("recording receiver uptime: %w", err)
	}
	return nil
}

// ActiveTimes returns the Claude Code active time overlapping [from, to), as
// the struggle engine takes it: one entry per stored point, with UserSecs or
// CLISecs set by its type, sorted by (From, To, SessionID, type). Points
// with zero seconds are left out.
//
// ProjectCwd is the latest working directory Claude Code's hooks reported
// for the session before to ("" when hooks never fired for it);
// OpenTelemetry itself carries no directory.
//
// Claude Code sends nothing for idle intervals, so no data looks the same
// as zero active time. A session is therefore returned only when the agent's
// OTLP receiver was running (MarkReceiverUp) all through the session's span
// in the window: its hook events and its points. Otherwise its exports may
// have been lost while the agent was stopped, and the session is left out
// entirely, so the engine doesn't judge it on incomplete data.
func (d *DB) ActiveTimes(ctx context.Context, from, to time.Time) ([]struggle.ActiveTime, error) {
	f, t := from.UnixMilli(), to.UnixMilli()
	out, err := d.activePoints(ctx, f, t)
	if err != nil || len(out) == 0 {
		return out, err
	}
	if out, err = d.coveredOnly(ctx, out, from, to); err != nil {
		return nil, err
	}
	cwds := map[string]string{}
	for i := range out {
		id := out[i].SessionID
		cwd, ok := cwds[id]
		if !ok {
			if cwd, err = d.sessionCwd(ctx, id, t); err != nil {
				return nil, err
			}
			cwds[id] = cwd
		}
		out[i].ProjectCwd = cwd
	}
	return out, nil
}

// span is a time range [From, To).
type span struct{ From, To time.Time }

// coveredOnly drops the points of every session whose span in [from, to)
// touches a time the receiver wasn't running.
func (d *DB) coveredOnly(ctx context.Context, pts []struggle.ActiveTime, from, to time.Time) ([]struggle.ActiveTime, error) {
	runs, err := d.receiverRuns(ctx, from.Add(-coverAfter), to.Add(coverBefore))
	if err != nil {
		return nil, err
	}
	holes := uncovered(runs, from, to)
	if len(holes) == 0 {
		return pts, nil
	}
	spans := map[string]span{}
	var order []string
	for _, p := range pts {
		s, ok := spans[p.SessionID]
		if !ok {
			order = append(order, p.SessionID)
			s = span{From: p.From, To: p.To}
		}
		spans[p.SessionID] = span{From: minTime(s.From, p.From), To: maxTime(s.To, p.To)}
	}
	drop := map[string]bool{}
	for _, id := range order {
		s := spans[id]
		hooks, ok, err := d.sessionSpan(ctx, id, from, to)
		if err != nil {
			return nil, err
		}
		if ok {
			s = span{From: minTime(s.From, hooks.From), To: maxTime(s.To, hooks.To)}
		}
		s = span{From: maxTime(s.From, from), To: minTime(s.To, to)}
		drop[id] = touches(s, holes)
	}
	kept := pts[:0]
	for _, p := range pts {
		if !drop[p.SessionID] {
			kept = append(kept, p)
		}
	}
	return kept, nil
}

// receiverRuns lists the receiver runs overlapping [from, to].
func (d *DB) receiverRuns(ctx context.Context, from, to time.Time) ([]span, error) {
	rows, err := d.sql.QueryContext(ctx, qUptimeRange, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("reading receiver uptime: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []span
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("scanning receiver uptime: %w", err)
		}
		out = append(out, span{From: time.UnixMilli(a).UTC(), To: time.UnixMilli(b).UTC()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading receiver uptime: %w", err)
	}
	return out, nil
}

// sessionSpan is the first and last Claude Code hook event of a session in
// [from, to); ok is false when there is none.
func (d *DB) sessionSpan(ctx context.Context, sessionID string, from, to time.Time) (span, bool, error) {
	var a, b sql.NullInt64
	if err := d.sql.QueryRowContext(ctx, qSessionSpan, sessionID, from.UnixMilli(), to.UnixMilli()).Scan(&a, &b); err != nil {
		return span{}, false, fmt.Errorf("reading session span: %w", err)
	}
	if !a.Valid || !b.Valid {
		return span{}, false, nil
	}
	return span{From: time.UnixMilli(a.Int64).UTC(), To: time.UnixMilli(b.Int64).UTC()}, true, nil
}

// uncovered returns the parts of [from, to) that no receiver run covers,
// each run widened to [From-coverBefore, To+coverAfter]. runs must be sorted
// by From. It is pure.
func uncovered(runs []span, from, to time.Time) []span {
	var holes []span
	cur := from // everything before cur is covered or already a hole
	for _, r := range runs {
		a, b := r.From.Add(-coverBefore), r.To.Add(coverAfter)
		if !b.After(cur) {
			continue
		}
		if a.After(cur) {
			holes = append(holes, span{From: cur, To: minTime(a, to)})
		}
		cur = b
		if !cur.Before(to) {
			break
		}
	}
	if cur.Before(to) {
		holes = append(holes, span{From: cur, To: to})
	}
	return holes
}

// touches reports whether the closed span s meets any hole [From, To).
func touches(s span, holes []span) bool {
	for _, h := range holes {
		if s.From.Before(h.To) && !h.From.After(s.To) {
			return true
		}
	}
	return false
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// sessionCwd is the latest working directory Claude Code's hooks reported
// for a session before t (unix ms), or "".
func (d *DB) sessionCwd(ctx context.Context, sessionID string, t int64) (string, error) {
	var cwd string
	err := d.sql.QueryRowContext(ctx, qSessionCwd, sessionID, t).Scan(&cwd)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading session directory: %w", err)
	}
	return cwd, nil
}

// activePoints reads the stored points overlapping [f, t) (unix ms).
func (d *DB) activePoints(ctx context.Context, f, t int64) ([]struggle.ActiveTime, error) {
	rows, err := d.sql.QueryContext(ctx, qActiveRange, f, t, f)
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
		if err := rows.Scan(&fromMS, &toMS, &at.SessionID, &typ, &secs); err != nil {
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
