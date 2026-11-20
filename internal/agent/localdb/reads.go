package localdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Counts are the number of locally recorded items since a time.
type Counts struct {
	Heartbeats      int64 `json:"heartbeats"`
	AIEvents        int64 `json:"ai_events"`
	ActivitySamples int64 `json:"activity_samples"`
	GitEvents       int64 `json:"git_events"`
}

// CountSince counts heartbeats and raw events recorded at or after since.
func (d *DB) CountSince(ctx context.Context, since time.Time) (Counts, error) {
	ms := since.UnixMilli()
	var c Counts
	for _, q := range []struct {
		query string
		dst   *int64
	}{
		{qLogCount, &c.Heartbeats}, {qAICount, &c.AIEvents},
		{qActivityCount, &c.ActivitySamples}, {qGitCount, &c.GitEvents},
	} {
		if err := d.sql.QueryRowContext(ctx, q.query, ms).Scan(q.dst); err != nil {
			return Counts{}, fmt.Errorf("counting local events: %w", err)
		}
	}
	return c, nil
}

// HeartbeatTimes returns the times of logged heartbeats at or after since,
// oldest first. The log keeps two days, so the result is bounded.
func (d *DB) HeartbeatTimes(ctx context.Context, since time.Time) ([]time.Time, error) {
	rows, err := d.sql.QueryContext(ctx, qLogTimes, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("reading heartbeat log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []time.Time
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return nil, fmt.Errorf("scanning heartbeat log: %w", err)
		}
		out = append(out, time.UnixMilli(ms).UTC())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading heartbeat log: %w", err)
	}
	return out, nil
}

// DirSeen is a directory seen in a heartbeat or AI event.
type DirSeen struct {
	Dir     string
	Project string // may be empty
	Last    time.Time
}

// RecentDirs returns up to limit distinct directories seen in heartbeats or
// AI events at or after since, most recent first.
func (d *DB) RecentDirs(ctx context.Context, since time.Time, limit int) ([]DirSeen, error) {
	ms := since.UnixMilli()
	rows, err := d.sql.QueryContext(ctx, qRecentDirs, ms, ms, limit)
	if err != nil {
		return nil, fmt.Errorf("reading recent dirs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DirSeen
	for rows.Next() {
		var (
			s    DirSeen
			last sql.NullInt64
		)
		if err := rows.Scan(&s.Dir, &s.Project, &last); err != nil {
			return nil, fmt.Errorf("scanning recent dirs: %w", err)
		}
		s.Last = time.UnixMilli(last.Int64).UTC()
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading recent dirs: %w", err)
	}
	return out, nil
}

// PruneResult counts rows removed by Prune.
type PruneResult struct{ AIEvents, ActivitySamples, GitEvents, HeartbeatLog int64 }

// Prune deletes raw events older than the retention period and heartbeat
// log entries older than two days.
func (d *DB) Prune(ctx context.Context) (PruneResult, error) {
	now := d.now()
	cut := now.Add(-d.retention).UnixMilli()
	logCut := now.Add(-d.logRetention).UnixMilli()
	var r PruneResult
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		for _, p := range []struct {
			query string
			cut   int64
			dst   *int64
		}{
			{qAIPrune, cut, &r.AIEvents}, {qActivityPrune, cut, &r.ActivitySamples},
			{qGitPrune, cut, &r.GitEvents}, {qLogPrune, logCut, &r.HeartbeatLog},
		} {
			res, err := tx.ExecContext(ctx, p.query, p.cut)
			if err != nil {
				return fmt.Errorf("pruning: %w", err)
			}
			if *p.dst, err = res.RowsAffected(); err != nil {
				return fmt.Errorf("pruning: %w", err)
			}
		}
		return nil
	})
	return r, err
}
