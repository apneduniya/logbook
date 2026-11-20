package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// CreditedMinute is one row of the engine's output as kept locally.
type CreditedMinute struct {
	Minute       time.Time // UTC, whole minute
	Project      string
	Label        string
	Credited     int
	Cuts, Flags  []string
	RulesVersion string
	// Digest identifies the row's content; the caller computes it.
	Digest string
}

// MinuteUpdate is one recompute of the engine over [From, To).
type MinuteUpdate struct {
	From, To time.Time
	// OpenFrom splits the window: minutes at or after it are still open and
	// are rewritten; minutes before it are final and only added if missing.
	OpenFrom time.Time
	Minutes  []CreditedMinute
	// Superseded returns the row that replaces an open minute's row the new
	// result no longer contains, once the server has seen it. Rows the
	// server never saw are simply deleted.
	Superseded func(old CreditedMinute) CreditedMinute
}

// SaveResult counts what SaveMinutes changed.
type SaveResult struct{ Inserted, Updated, Deleted int }

type minuteKey struct {
	minute  int64
	project string
}

type minuteState struct{ digest, sentDigest string }

// SaveMinutes applies one engine recompute in a transaction: open minutes are
// inserted or rewritten when their digest changed, final minutes are only
// inserted when missing, and open rows missing from the new result are
// superseded (if the server saw them) or deleted.
func (d *DB) SaveMinutes(ctx context.Context, u MinuteUpdate) (SaveResult, error) {
	var res SaveResult
	open := u.OpenFrom.Unix()
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		have, err := minuteStates(ctx, tx, u.From, u.To)
		if err != nil {
			return err
		}
		seen := make(map[minuteKey]bool, len(u.Minutes))
		for _, m := range u.Minutes {
			k := minuteKey{m.Minute.Unix(), m.Project}
			seen[k] = true
			old, exists := have[k]
			switch {
			case !exists:
				if err := writeMinute(ctx, tx, qMinuteInsert, m); err != nil {
					return err
				}
				res.Inserted++
			case k.minute >= open && old.digest != m.Digest:
				if err := writeMinute(ctx, tx, qMinuteUpdate, m); err != nil {
					return err
				}
				res.Updated++
			}
		}
		// Iterating a map is fine here: every key is handled independently.
		for k, old := range have {
			if seen[k] || k.minute < open {
				continue
			}
			if old.sentDigest == "" {
				if _, err := tx.ExecContext(ctx, qMinuteDelete, k.minute, k.project); err != nil {
					return fmt.Errorf("deleting credited minute: %w", err)
				}
				res.Deleted++
				continue
			}
			z := u.Superseded(CreditedMinute{Minute: time.Unix(k.minute, 0).UTC(), Project: k.project, Digest: old.digest})
			if z.Digest == old.digest {
				continue
			}
			if err := writeMinute(ctx, tx, qMinuteUpdate, z); err != nil {
				return err
			}
			res.Updated++
		}
		return nil
	})
	return res, err
}

// minuteStates reads the digests of the stored rows in [from, to).
func minuteStates(ctx context.Context, tx *sql.Tx, from, to time.Time) (map[minuteKey]minuteState, error) {
	rows, err := tx.QueryContext(ctx, qMinutesInRange, from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("reading credited minutes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	have := map[minuteKey]minuteState{}
	for rows.Next() {
		var (
			k minuteKey
			s minuteState
		)
		if err := rows.Scan(&k.minute, &k.project, &s.digest, &s.sentDigest); err != nil {
			return nil, fmt.Errorf("scanning credited minutes: %w", err)
		}
		have[k] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading credited minutes: %w", err)
	}
	return have, nil
}

// writeMinute runs qMinuteInsert or qMinuteUpdate for m.
func writeMinute(ctx context.Context, tx *sql.Tx, q string, m CreditedMinute) error {
	cuts, err := codesJSON(m.Cuts)
	if err != nil {
		return err
	}
	flags, err := codesJSON(m.Flags)
	if err != nil {
		return err
	}
	var args []any
	if q == qMinuteInsert {
		args = []any{m.Minute.Unix(), m.Project, m.Label, m.Credited, cuts, flags, m.RulesVersion, m.Digest}
	} else {
		args = []any{m.Label, m.Credited, cuts, flags, m.RulesVersion, m.Digest, m.Minute.Unix(), m.Project}
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("writing credited minute: %w", err)
	}
	return nil
}

func codesJSON(codes []string) (string, error) {
	if codes == nil {
		codes = []string{}
	}
	b, err := json.Marshal(codes)
	if err != nil {
		return "", fmt.Errorf("encoding codes: %w", err)
	}
	return string(b), nil
}

// UnsentMinutes returns up to limit rows whose current content the server
// hasn't accepted yet, oldest minute first and, within a minute, smallest
// credit first (so a project losing credit is sent before one gaining it).
// Final minutes (before finalBefore) whose content the server already has
// are first marked done.
func (d *DB) UnsentMinutes(ctx context.Context, finalBefore time.Time, limit int) ([]CreditedMinute, error) {
	if _, err := d.sql.ExecContext(ctx, qMinutesSettleFinal, finalBefore.Unix()); err != nil {
		return nil, fmt.Errorf("settling final minutes: %w", err)
	}
	var out []CreditedMinute
	err := d.query(ctx, qMinutesUnsent, []any{limit}, func(r *sql.Rows) error {
		var (
			m           CreditedMinute
			at          int64
			cuts, flags string
		)
		if err := r.Scan(&at, &m.Project, &m.Label, &m.Credited, &cuts, &flags, &m.RulesVersion, &m.Digest); err != nil {
			return err
		}
		m.Minute = time.Unix(at, 0).UTC()
		if err := json.Unmarshal([]byte(cuts), &m.Cuts); err != nil {
			return fmt.Errorf("decoding cuts: %w", err)
		}
		if err := json.Unmarshal([]byte(flags), &m.Flags); err != nil {
			return fmt.Errorf("decoding flags: %w", err)
		}
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading unsent minutes: %w", err)
	}
	return out, nil
}

// MarkMinutesSent records that the server has answered for ms (accepted or
// rejected for good). A row whose content changed since it was read stays
// unsent. Rows before finalBefore are marked final.
func (d *DB) MarkMinutesSent(ctx context.Context, ms []CreditedMinute, finalBefore time.Time) error {
	if len(ms) == 0 {
		return nil
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		return execEach(ctx, tx, qMinuteMarkSent, len(ms), func(i int) []any {
			final := 0
			if ms[i].Minute.Before(finalBefore) {
				final = 1
			}
			return []any{final, ms[i].Minute.Unix(), ms[i].Project, ms[i].Digest}
		})
	})
}

// RetireMinutesBefore marks every unsent minute before t as done without
// sending it (used when the device identity changes).
func (d *DB) RetireMinutesBefore(ctx context.Context, t time.Time) error {
	if _, err := d.sql.ExecContext(ctx, qMinutesRetire, t.Unix()); err != nil {
		return fmt.Errorf("retiring credited minutes: %w", err)
	}
	return nil
}

// UnsentMinuteCount counts rows waiting to be sent.
func (d *DB) UnsentMinuteCount(ctx context.Context) (int64, error) {
	var n int64
	if err := d.sql.QueryRowContext(ctx, qMinutesUnsentCount).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting unsent minutes: %w", err)
	}
	return n, nil
}

// CreditPruneResult counts rows removed by PruneCrediting.
type CreditPruneResult struct{ Heartbeats, CreditedMinutes int64 }

// PruneCrediting deletes full heartbeats and credited minutes older than the
// retention period (90 days by default). Evidence days are kept: one small
// row per day.
func (d *DB) PruneCrediting(ctx context.Context) (CreditPruneResult, error) {
	cut := d.now().Add(-d.retention)
	var r CreditPruneResult
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, qHeartbeatPrune, cut.UnixMilli())
		if err != nil {
			return fmt.Errorf("pruning heartbeats: %w", err)
		}
		if r.Heartbeats, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("pruning heartbeats: %w", err)
		}
		res, err = tx.ExecContext(ctx, qMinutesPrune, cut.Unix())
		if err != nil {
			return fmt.Errorf("pruning credited minutes: %w", err)
		}
		if r.CreditedMinutes, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("pruning credited minutes: %w", err)
		}
		return nil
	})
	return r, err
}
