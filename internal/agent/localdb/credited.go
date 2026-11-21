package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"
)

// Key is the computation key of a credited minute (ADR 0017): the engine
// ran over the window ending at T (a whole minute) on the event log at its
// high-water mark H. A minute's value is the deterministic function of its
// key, so a replay at the key reproduces it. The zero Key means unkeyed: a
// row stored before keys existed, which is never recomputed.
type Key struct {
	T time.Time
	H int64
}

// IsZero reports whether k is unkeyed.
func (k Key) IsZero() bool { return k.T.IsZero() }

// args returns k as the computed_at and snapshot columns (NULL if unkeyed).
func (k Key) args() (any, any) {
	if k.IsZero() {
		return nil, nil
	}
	return k.T.Unix(), k.H
}

// CreditedMinute is one row of the engine's output as kept locally. All rows
// of one minute carry the same key and state: the minute is one unit.
type CreditedMinute struct {
	Minute       time.Time // UTC, whole minute
	Project      string
	Label        string
	Credited     int
	Cuts, Flags  []string
	RulesVersion string
	// Digest identifies the row's content; the caller computes it.
	Digest string
	// Key produced the row. SaveRun and AdoptMinute set it.
	Key Key
	// Final rows are never recomputed. Acknowledged rows are held by the
	// server as they are (or were refused for good) and are not sent again.
	// Both are stored state; callers writing rows leave them unset.
	Final, Acknowledged bool
}

// storedRow is a stored row plus its sent_digest: what the server last
// accepted for its (minute, project), content and key; "?" when a row was
// sent but the answer is unknown; "" if the server can't hold a row.
type storedRow struct {
	CreditedMinute
	sent string
}

// Run is one engine run to store (ADR 0017).
type Run struct {
	// Key is the run's computation key.
	Key Key
	// From and To bound the minutes the run computed and may write.
	From, To time.Time
	// Minutes is the run's result: rows in [From, To) with their digests.
	Minutes []CreditedMinute
	// Removal returns the row that removes project from minute on the
	// server: sent when an open minute's new result no longer has a
	// project the server holds.
	Removal func(minute time.Time, project string) CreditedMinute
	// FinalBefore, when set, makes every open minute before it final in
	// the same transaction (a settled run with T >= minute + the final
	// lead). The minutes [From, FinalBefore) become final whether or not
	// they have rows.
	FinalBefore time.Time
}

// RunResult counts what SaveRun did, in minutes.
type RunResult struct{ Replaced, Kept, Finalized int }

// SaveRun stores one engine run in a transaction. Final minutes are never
// touched. For every other minute in [From, To): when the run's rows (plus
// a removal for each project the server holds that the result dropped)
// equal the stored rows, the stored rows and their older key are kept, so
// unchanged minutes are never re-sent; otherwise all of the minute's rows
// are replaced with the new ones and the run's key, open and not
// acknowledged. Then FinalBefore, if set, makes minutes final.
func (d *DB) SaveRun(ctx context.Context, r Run) (RunResult, error) {
	var res RunResult
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		final, err := finalSpans(ctx, tx, r.From, r.To)
		if err != nil {
			return err
		}
		stored, err := readMinutes(ctx, tx, r.From, r.To)
		if err != nil {
			return err
		}
		have := map[int64][]storedRow{}
		for _, s := range stored {
			have[s.Minute.Unix()] = append(have[s.Minute.Unix()], s)
		}
		want := map[int64][]CreditedMinute{}
		for _, m := range r.Minutes {
			if at := m.Minute.Unix(); !m.Minute.Before(r.From) && m.Minute.Before(r.To) {
				want[at] = append(want[at], m)
			}
		}
		for m := r.From; m.Before(r.To); m = m.Add(time.Minute) {
			at := m.Unix()
			old := have[at]
			if containsMinute(final, m) || slices.ContainsFunc(old, func(s storedRow) bool { return s.Final }) {
				continue
			}
			next := withRemovals(want[at], old, r.Removal)
			if len(old) == 0 && len(next) == 0 {
				continue
			}
			if sameRows(old, next) {
				res.Kept++
				continue
			}
			if err := replaceMinute(ctx, tx, m, old, next, r.Key); err != nil {
				return err
			}
			res.Replaced++
		}
		if r.FinalBefore.IsZero() {
			return nil
		}
		res.Finalized, err = finalize(ctx, tx, r.From, r.FinalBefore)
		return err
	})
	return res, err
}

// withRemovals is the rows a minute should hold: the result, plus a removal
// for each project the server holds that the result no longer has.
func withRemovals(result []CreditedMinute, old []storedRow, removal func(time.Time, string) CreditedMinute) []CreditedMinute {
	next := slices.Clone(result)
	for _, o := range old {
		if o.sent == "" || slices.ContainsFunc(result, func(m CreditedMinute) bool { return m.Project == o.Project }) {
			continue
		}
		next = append(next, removal(o.Minute, o.Project))
	}
	return next
}

// sameRows reports whether a minute's keyed stored rows hold exactly next.
func sameRows(old []storedRow, next []CreditedMinute) bool {
	if len(old) != len(next) {
		return false
	}
	for _, n := range next {
		i := slices.IndexFunc(old, func(o storedRow) bool { return o.Project == n.Project })
		if i < 0 || old[i].Digest != n.Digest || old[i].Key.IsZero() {
			return false
		}
	}
	return true
}

// replaceMinute replaces all of minute m's rows with next, under key. Each
// row keeps what the server last accepted for its project.
func replaceMinute(ctx context.Context, tx *sql.Tx, m time.Time, old []storedRow, next []CreditedMinute, key Key) error {
	if _, err := tx.ExecContext(ctx, qMinuteClear, m.Unix()); err != nil {
		return fmt.Errorf("replacing credited minute: %w", err)
	}
	for _, n := range next {
		sent := ""
		if i := slices.IndexFunc(old, func(o storedRow) bool { return o.Project == n.Project }); i >= 0 {
			sent = old[i].sent
		}
		n.Key, n.Final, n.Acknowledged = key, false, false
		if err := insertMinute(ctx, tx, n, sent); err != nil {
			return err
		}
	}
	return nil
}

// finalize makes every open minute before `before` final and records the
// final spans: [from, before), where the run computed, and each older
// minute that had open rows. It returns how many minutes with rows became
// final.
func finalize(ctx context.Context, tx *sql.Tx, from, before time.Time) (int, error) {
	var open []int64
	err := queryRows(ctx, tx, qMinutesOpenBefore, []any{before.Unix()}, func(r *sql.Rows) error {
		var m int64
		if err := r.Scan(&m); err != nil {
			return err
		}
		open = append(open, m)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("reading open minutes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, qMinutesFinalize, before.Unix()); err != nil {
		return 0, fmt.Errorf("finalizing minutes: %w", err)
	}
	if before.After(from) {
		if err := addSpan(ctx, tx, from, before); err != nil {
			return 0, err
		}
	}
	for _, m := range open {
		if t := time.Unix(m, 0).UTC(); t.Before(from) {
			if err := addSpan(ctx, tx, t, t.Add(time.Minute)); err != nil {
				return 0, err
			}
		}
	}
	return len(open), nil
}

// AdoptMinute replaces minute's rows with rows exactly as the server holds
// them (values and keys; ADR 0017), final and acknowledged. rows may be
// empty: the server holds nothing for the minute.
func (d *DB) AdoptMinute(ctx context.Context, minute time.Time, rows []CreditedMinute) error {
	return d.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, qMinuteClear, minute.Unix()); err != nil {
			return fmt.Errorf("adopting credited minute: %w", err)
		}
		for _, m := range rows {
			m.Minute, m.Final, m.Acknowledged = minute, true, true
			if err := insertMinute(ctx, tx, m, ""); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, qMinuteHeld, minute.Unix()); err != nil {
			return fmt.Errorf("adopting credited minute: %w", err)
		}
		return addSpan(ctx, tx, minute, minute.Add(time.Minute))
	})
}

// IsFinal reports whether minute is final locally.
func (d *DB) IsFinal(ctx context.Context, minute time.Time) (bool, error) {
	spans, err := finalSpans(ctx, d.sql, minute, minute.Add(time.Minute))
	return len(spans) > 0, err
}

// finalSpans returns the final spans overlapping [from, to), oldest first.
func finalSpans(ctx context.Context, tx queryer, from, to time.Time) ([]span, error) {
	var out []span
	err := queryRows(ctx, tx, qSpansOverlap, []any{from.Unix(), to.Unix()}, func(r *sql.Rows) error {
		var a, b int64
		if err := r.Scan(&a, &b); err != nil {
			return err
		}
		out = append(out, span{From: time.Unix(a, 0).UTC(), To: time.Unix(b, 0).UTC()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading final minutes: %w", err)
	}
	return out, nil
}

func containsMinute(spans []span, m time.Time) bool {
	return slices.ContainsFunc(spans, func(s span) bool { return !m.Before(s.From) && m.Before(s.To) })
}

// addSpan records [from, to) as final, merged with the spans it touches.
func addSpan(ctx context.Context, tx *sql.Tx, from, to time.Time) error {
	lo, hi := from.Unix(), to.Unix()
	var touching []int64
	err := queryRows(ctx, tx, qSpansTouching, []any{lo, hi}, func(r *sql.Rows) error {
		var a, b int64
		if err := r.Scan(&a, &b); err != nil {
			return err
		}
		touching = append(touching, a)
		lo, hi = min(lo, a), max(hi, b)
		return nil
	})
	if err != nil {
		return fmt.Errorf("reading final minutes: %w", err)
	}
	for _, a := range touching {
		if _, err := tx.ExecContext(ctx, qSpanDelete, a); err != nil {
			return fmt.Errorf("merging final minutes: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, qSpanInsert, lo, hi); err != nil {
		return fmt.Errorf("recording final minutes: %w", err)
	}
	return nil
}

// readMinutes reads the stored rows of [from, to).
func readMinutes(ctx context.Context, tx *sql.Tx, from, to time.Time) ([]storedRow, error) {
	var out []storedRow
	err := queryRows(ctx, tx, qMinutesInRange, []any{from.Unix(), to.Unix()}, func(r *sql.Rows) error {
		s, err := scanMinute(r)
		out = append(out, s)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reading credited minutes: %w", err)
	}
	return out, nil
}

// scanMinute reads one row of minuteColumns.
func scanMinute(r *sql.Rows) (storedRow, error) {
	var (
		s               storedRow
		at              int64
		cuts, flags     string
		state           string
		computed, snap  sql.NullInt64
		acknowledgedInt int
	)
	if err := r.Scan(&at, &s.Project, &s.Label, &s.Credited, &cuts, &flags, &s.RulesVersion, &s.Digest,
		&s.sent, &computed, &snap, &state, &acknowledgedInt); err != nil {
		return storedRow{}, err
	}
	s.Minute = time.Unix(at, 0).UTC()
	if computed.Valid && snap.Valid {
		s.Key = Key{T: time.Unix(computed.Int64, 0).UTC(), H: snap.Int64}
	}
	s.Final, s.Acknowledged = state == "final", acknowledgedInt == 1
	if err := json.Unmarshal([]byte(cuts), &s.Cuts); err != nil {
		return storedRow{}, fmt.Errorf("decoding cuts: %w", err)
	}
	if err := json.Unmarshal([]byte(flags), &s.Flags); err != nil {
		return storedRow{}, fmt.Errorf("decoding flags: %w", err)
	}
	return s, nil
}

// insertMinute stores m with its key and state; sent is its sent_digest.
func insertMinute(ctx context.Context, tx *sql.Tx, m CreditedMinute, sent string) error {
	cuts, err := codesJSON(m.Cuts)
	if err != nil {
		return err
	}
	flags, err := codesJSON(m.Flags)
	if err != nil {
		return err
	}
	state := "open"
	if m.Final {
		state = "final"
	}
	acknowledged := 0
	if m.Acknowledged {
		acknowledged = 1
	}
	t, h := m.Key.args()
	if _, err := tx.ExecContext(ctx, qMinuteInsert, m.Minute.Unix(), m.Project, m.Label, m.Credited, cuts, flags,
		m.RulesVersion, m.Digest, sent, t, h, state, acknowledged); err != nil {
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

// StoredMinutes returns every stored row in [from, to), with its key and
// state, sorted by minute and project.
func (d *DB) StoredMinutes(ctx context.Context, from, to time.Time) ([]CreditedMinute, error) {
	var out []CreditedMinute
	err := queryRows(ctx, d.sql, qMinutesInRange, []any{from.Unix(), to.Unix()}, func(r *sql.Rows) error {
		s, err := scanMinute(r)
		out = append(out, s.CreditedMinute)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reading credited minutes: %w", err)
	}
	return out, nil
}

// UnsentMinutes returns rows the server hasn't answered for as they now
// are, oldest minute first and, within a minute, smallest credit first (so
// a project losing credit is sent before one gaining it). Open minutes
// before holdOpenBefore are left out: they are held while the agent
// settles and must not reach the server before they are final (ADR 0017).
// It returns whole minutes only: at most limit rows, unless the first
// minute alone has more, in which case that minute is returned whole. A
// minute's rows are never split across batches: they are one unit, and the
// server keeps its 60-second cap only by applying a minute's decreases and
// increases in one request.
//
// Only minutes after `after` are read (zero: all), so a caller sending
// batches in order never reads a minute twice in one pass.
func (d *DB) UnsentMinutes(ctx context.Context, after, holdOpenBefore time.Time, limit int) ([]CreditedMinute, error) {
	if limit < 1 {
		limit = 1
	}
	unix := func(t time.Time) int64 {
		if t.IsZero() {
			return math.MinInt64
		}
		return t.Unix()
	}
	var out []CreditedMinute
	err := queryRows(ctx, d.sql, qMinutesUnsent, []any{unix(after), unix(holdOpenBefore), limit - 1}, func(r *sql.Rows) error {
		s, err := scanMinute(r)
		out = append(out, s.CreditedMinute)
		return err
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

// MinutesAnswer is the server's answer for rows sent as read.
type MinutesAnswer struct {
	// Accepted rows are held by the server as they are. An accepted
	// removal is deleted here too, since the server deleted its row.
	Accepted []CreditedMinute
	// Answered rows were refused while open. They are not sent again while
	// open, and are sent once more when they become final.
	Answered []CreditedMinute
	// Dropped rows were refused for good, without the server's rows: the
	// server holds none for them, so they are deleted, and their minutes
	// are final (they already are, unless the day was sealed there).
	Dropped []CreditedMinute
}

// MarkMinutesSent records the server's answer for rows sent as read. A row
// that changed since it was read (content or key) is left alone.
func (d *DB) MarkMinutesSent(ctx context.Context, a MinutesAnswer) error {
	if len(a.Accepted)+len(a.Answered)+len(a.Dropped) == 0 {
		return nil
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []struct {
			query string
			rows  []CreditedMinute
		}{{qMinuteAccepted, a.Accepted}, {qMinuteRemoved, a.Accepted}, {qMinuteAnswered, a.Answered}, {qMinuteDropped, a.Dropped}} {
			if err := execEach(ctx, tx, q.query, len(q.rows), func(i int) []any {
				m := q.rows[i]
				t, h := m.Key.args()
				return []any{m.Minute.Unix(), m.Project, m.Digest, t, h}
			}); err != nil {
				return fmt.Errorf("recording sent minutes: %w", err)
			}
		}
		for _, m := range a.Dropped {
			if err := addSpan(ctx, tx, m.Minute, m.Minute.Add(time.Minute)); err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkMinutesTried records, before rows are sent, that the server may hold
// a row for each of their (minute, project) pairs: if the answer is lost,
// a later result without that project still sends a removal for it rather
// than leaving the server with a row the agent doesn't know.
func (d *DB) MarkMinutesTried(ctx context.Context, ms []CreditedMinute) error {
	if len(ms) == 0 {
		return nil
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		if err := execEach(ctx, tx, qMinuteTried, len(ms), func(i int) []any {
			return []any{ms[i].Minute.Unix(), ms[i].Project}
		}); err != nil {
			return fmt.Errorf("recording sent minutes: %w", err)
		}
		return nil
	})
}

// RetireMinutesBefore hands every minute before t to the previous device
// (used when the device identity changes): those minutes are never sent
// again and never recomputed. Rows the previous device holds exactly as
// stored are kept, final and acknowledged, and so are rows from before
// keys (migration 0007), which it held as far as the agent can know. Any
// other row is deleted: the agent can't know what, if anything, that
// device holds for it, and it never shows a final value the server doesn't
// hold (ADR 0017). All of
// [0, t) becomes final, so no later run (after a clock rollback, say)
// computes those minutes again for the new device.
func (d *DB) RetireMinutesBefore(ctx context.Context, t time.Time) error {
	return d.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, qMinutesRetireUnheld, t.Unix()); err != nil {
			return fmt.Errorf("retiring credited minutes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, qMinutesRetire, t.Unix()); err != nil {
			return fmt.Errorf("retiring credited minutes: %w", err)
		}
		return addSpan(ctx, tx, time.Unix(0, 0), t)
	})
}

// UnsentMinuteCount counts rows waiting for the server's answer.
func (d *DB) UnsentMinuteCount(ctx context.Context) (int64, error) {
	var n int64
	if err := d.sql.QueryRowContext(ctx, qMinutesUnacked).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting unsent minutes: %w", err)
	}
	return n, nil
}

// MinutesPendingBefore counts rows before t that are open or waiting for
// the server's answer. Zero means every stored minute before t is final
// and acknowledged.
func (d *DB) MinutesPendingBefore(ctx context.Context, t time.Time) (int64, error) {
	var n int64
	if err := d.sql.QueryRowContext(ctx, qMinutesPendingBefore, t.Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting pending minutes: %w", err)
	}
	return n, nil
}

// CreditPruneResult counts rows removed by PruneCrediting.
type CreditPruneResult struct{ Heartbeats, CreditedMinutes int64 }

// PruneCrediting deletes, after the retention period (90 days by default):
// full heartbeats the evidence chain has sealed, credited minutes and
// their final spans, and sealed repo mapping rows replaced by a newer row
// for the same path. Evidence days are kept (one small row per day).
func (d *DB) PruneCrediting(ctx context.Context) (CreditPruneResult, error) {
	cut := d.now().Add(-d.retention)
	var r CreditPruneResult
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		sealed, err := sealedSeq(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, qHeartbeatPrune, cut.UnixMilli(), sealed)
		if err != nil {
			return fmt.Errorf("pruning heartbeats: %w", err)
		}
		if r.Heartbeats, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("pruning heartbeats: %w", err)
		}
		res, err = tx.ExecContext(ctx, qRepoPrune, sealed, cut.UnixMilli())
		if err != nil {
			return fmt.Errorf("pruning repo mapping: %w", err)
		}
		repos, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("pruning repo mapping: %w", err)
		}
		if err := bumpGeneration(ctx, tx, r.Heartbeats+repos, d.now()); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, qMinutesPrune, cut.Unix())
		if err != nil {
			return fmt.Errorf("pruning credited minutes: %w", err)
		}
		if r.CreditedMinutes, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("pruning credited minutes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, qSpansPrune, cut.Unix()); err != nil {
			return fmt.Errorf("pruning final minutes: %w", err)
		}
		return nil
	})
	return r, err
}
