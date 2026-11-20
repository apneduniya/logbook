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
	// result no longer contains, once the server has seen it (a removal,
	// for the server). Rows the server never saw are simply deleted.
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
// inserted or rewritten when their digest changed, and open rows missing
// from the new result are superseded (if the server saw them) or deleted.
// A final minute is frozen whole: its rows are never rewritten and it takes
// no new project. Only a final minute with no rows at all is filled in
// (offline catch-up), so a minute is never credited twice under two
// projects.
func (d *DB) SaveMinutes(ctx context.Context, u MinuteUpdate) (SaveResult, error) {
	var res SaveResult
	open := u.OpenFrom.Unix()
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		have, err := minuteStates(ctx, tx, u.From, u.To)
		if err != nil {
			return err
		}
		stored := make(map[int64]bool, len(have)) // minutes with any row
		for k := range have {
			stored[k.minute] = true
		}
		seen := make(map[minuteKey]bool, len(u.Minutes))
		for _, m := range u.Minutes {
			k := minuteKey{m.Minute.Unix(), m.Project}
			seen[k] = true
			old, exists := have[k]
			switch {
			case k.minute < open && stored[k.minute] && !exists:
				// A final minute that already has rows: frozen.
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

// UnsentMinutes returns rows whose current content the server hasn't
// accepted yet, oldest minute first and, within a minute, smallest credit
// first (so a project losing credit is sent before one gaining it). It
// returns whole minutes only: at most limit rows, unless the first minute
// alone has more, in which case that minute is returned whole. A minute's
// rows are never split across batches, because the server can keep its
// 60-second cap only by applying a minute's decreases and increases in one
// request. Final minutes (before finalBefore) whose content the server
// already has are first marked done.
func (d *DB) UnsentMinutes(ctx context.Context, finalBefore time.Time, limit int) ([]CreditedMinute, error) {
	if limit < 1 {
		limit = 1
	}
	if _, err := d.sql.ExecContext(ctx, qMinutesSettleFinal, finalBefore.Unix()); err != nil {
		return nil, fmt.Errorf("settling final minutes: %w", err)
	}
	var out []CreditedMinute
	err := d.query(ctx, qMinutesUnsent, []any{limit - 1}, func(r *sql.Rows) error {
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
	return wholeMinutes(out, limit), nil
}

// wholeMinutes trims rows (ordered by minute) to at most limit rows without
// splitting a minute; a first minute longer than limit is kept whole.
func wholeMinutes(rows []CreditedMinute, limit int) []CreditedMinute {
	if len(rows) <= limit {
		return rows
	}
	last := rows[len(rows)-1].Minute
	n := len(rows)
	for n > 0 && rows[n-1].Minute.Equal(last) {
		n--
	}
	if n == 0 {
		return rows
	}
	return rows[:n]
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

// UnsentMinutesBefore counts rows before t waiting to be sent. Zero means
// the server has answered for every stored minute before t.
func (d *DB) UnsentMinutesBefore(ctx context.Context, t time.Time) (int64, error) {
	var n int64
	if err := d.sql.QueryRowContext(ctx, qMinutesUnsentBefore, t.Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting unsent minutes: %w", err)
	}
	return n, nil
}

// CreditPruneResult counts rows removed by PruneCrediting.
type CreditPruneResult struct{ Heartbeats, CreditedMinutes int64 }

// PruneCrediting deletes full heartbeats and credited minutes older than the
// retention period (90 days by default). Evidence days are kept (one small
// row per day), without the active-time records of days wholly before the
// cut. Repo mapping rows go once a newer row for the same path is older
// than the cut.
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
		// A day whose raw events are gone can't be rebuilt; its kept active
		// time goes with them.
		if _, err := tx.ExecContext(ctx, qEvidenceForget, cut.UTC().Format(time.DateOnly)); err != nil {
			return fmt.Errorf("pruning evidence active time: %w", err)
		}
		if _, err := tx.ExecContext(ctx, qRepoPrune, cut.UnixMilli()); err != nil {
			return fmt.Errorf("pruning repo mapping: %w", err)
		}
		return nil
	})
	return r, err
}
