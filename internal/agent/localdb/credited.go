package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// KeyFinalThrough is the kv key of the final watermark: every minute before
// it (UTC, RFC 3339) is final and frozen locally, and its arrival cutoff is
// recorded (CutoffsBetween). It only moves forward, in SaveMinutes.
const KeyFinalThrough = "crediting.final_through"

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
	// Cutoff is the arrival cutoff of the engine run that produced the row
	// (ADR 0016). SaveMinutes sets it; callers leave it zero.
	Cutoff time.Time
}

// Span is a range of minutes [From, To). A zero Span is empty.
type Span struct{ From, To time.Time }

// CutoffRange gives the minutes [From, To) the arrival cutoff Cutoff (zero:
// never computed). Only ranges whose cutoff differs from the default are
// stored.
type CutoffRange struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Cutoff time.Time `json:"cutoff"`
}

// MinuteUpdate is one engine run over [From, To) and, optionally, the
// minutes it makes final.
type MinuteUpdate struct {
	// From and To bound the minutes the engine computed.
	From, To time.Time
	// Cutoff is the run's arrival cutoff: the engine read the events stored
	// by then. Every row written or confirmed by the run carries it.
	Cutoff time.Time
	// ServerFinalBefore: a minute before it whose rows the server has seen
	// is final on the server, so it is frozen here too (with the cutoff of
	// the run that last wrote it), even if not final locally yet.
	ServerFinalBefore time.Time
	Minutes           []CreditedMinute
	// Superseded returns the row that replaces an open minute's row the new
	// result no longer contains, once the server has seen it (a removal,
	// for the server). Rows the server never saw are simply deleted.
	Superseded func(old CreditedMinute) CreditedMinute
	// FinalBefore, when after the stored final watermark (KeyFinalThrough),
	// makes every minute before it final, in the same transaction: the
	// watermark moves there and each newly final minute's cutoff is
	// recorded, as an exception when it isn't minute + Lead. Zero leaves
	// the watermark alone.
	FinalBefore time.Time
	// Lead is the usual delay from a minute to the cutoff it is finalized
	// with; such minutes record nothing.
	Lead time.Duration
	// Ceiling is the latest the watermark can be at this run's clock. A
	// stored watermark past it (and past From) means the system clock was
	// set back: the watermark moves back to the later of the two, and the
	// cutoffs recorded after that are forgotten, so the minutes the clock
	// now reaches again are computed again. Zero: no ceiling.
	Ceiling time.Time
}

// SaveResult counts what SaveMinutes changed. Rewound is set when the
// watermark moved back (Ceiling).
type SaveResult struct {
	Inserted, Updated, Deleted, Finalized int
	Rewound                               bool
}

type minuteKey struct {
	minute  int64
	project string
}

type minuteState struct {
	digest, sentDigest string
	cutoff             int64
}

// SaveMinutes applies one engine run in a transaction.
//
// A minute is frozen when it is final locally (before the stored watermark)
// or final on the server (before ServerFinalBefore with a row the server
// has seen): its rows are never rewritten and it takes no new project.
// Every other minute in [From, To) is rewritten from the result: rows are
// inserted or updated when their digest changed, rows missing from the new
// result are superseded (if the server saw them) or deleted, and all its
// rows take the run's cutoff.
//
// Then, if FinalBefore is past the watermark, the minutes in between
// become final, each with the cutoff it was computed with: this run's for
// a minute it rewrote, the stored rows' for a frozen minute or one outside
// the run, and none (zero) for a minute with no rows outside the run.
func (d *DB) SaveMinutes(ctx context.Context, u MinuteUpdate) (SaveResult, error) {
	var res SaveResult
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		ft, err := finalThrough(ctx, tx)
		if err != nil {
			return err
		}
		first := ft.IsZero()
		if first {
			// The first run ever: nothing before it was computed by this
			// agent, and nothing before it becomes final with a cutoff.
			ft = u.From
		}
		if back := laterOf(u.Ceiling, u.From); !u.Ceiling.IsZero() && ft.After(back) {
			if err := forgetCutoffs(ctx, tx, back); err != nil {
				return err
			}
			ft, first, res.Rewound = back, true, true
		}
		low := u.From
		if ft.Before(low) {
			low = ft
		}
		have, err := minuteStates(ctx, tx, low, u.To)
		if err != nil {
			return err
		}
		frozen := frozenMinutes(have, ft.Unix(), u.ServerFinalBefore.Unix())
		if err := d.rewrite(ctx, tx, u, have, frozen, ft.Unix(), &res); err != nil {
			return err
		}
		if !u.FinalBefore.After(ft) {
			if first {
				return setFinalThrough(ctx, tx, ft, d.now())
			}
			return nil
		}
		ranges := newCutoffs(have, frozen, u, ft)
		for _, r := range ranges {
			if err := addCutoff(ctx, tx, r); err != nil {
				return err
			}
		}
		res.Finalized = int(u.FinalBefore.Sub(ft) / time.Minute)
		return setFinalThrough(ctx, tx, u.FinalBefore, d.now())
	})
	return res, err
}

// frozenMinutes returns the minutes of have that are final locally (before
// ft) or on the server (before serverFinal with a row the server saw).
func frozenMinutes(have map[minuteKey]minuteState, ft, serverFinal int64) map[int64]bool {
	frozen := map[int64]bool{}
	for k, s := range have {
		if k.minute < ft || (k.minute < serverFinal && s.sentDigest != "") {
			frozen[k.minute] = true
		}
	}
	return frozen
}

// rewrite applies the run's result to every minute of [From, To) that is
// neither frozen nor before ft, stamping their rows with the run's cutoff.
func (d *DB) rewrite(ctx context.Context, tx *sql.Tx, u MinuteUpdate, have map[minuteKey]minuteState,
	frozen map[int64]bool, ft int64, res *SaveResult) error {
	from, to, cutoff := u.From.Unix(), u.To.Unix(), u.Cutoff.Unix()
	open := func(m int64) bool { return m >= from && m < to && m >= ft && !frozen[m] }
	seen := make(map[minuteKey]bool, len(u.Minutes))
	for _, m := range u.Minutes {
		k := minuteKey{m.Minute.Unix(), m.Project}
		if !open(k.minute) {
			continue
		}
		seen[k] = true
		m.Cutoff = u.Cutoff
		old, exists := have[k]
		switch {
		case !exists:
			if err := writeMinute(ctx, tx, qMinuteInsert, m); err != nil {
				return err
			}
			res.Inserted++
		case old.digest != m.Digest:
			if err := writeMinute(ctx, tx, qMinuteUpdate, m); err != nil {
				return err
			}
			res.Updated++
		case old.cutoff != cutoff:
			if _, err := tx.ExecContext(ctx, qMinuteCutoff, cutoff, k.minute, k.project); err != nil {
				return fmt.Errorf("writing credited minute cutoff: %w", err)
			}
		}
	}
	// Iterating a map is fine here: every key is handled independently.
	for k, old := range have {
		if seen[k] || !open(k.minute) {
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
		z.Cutoff = u.Cutoff
		if z.Digest == old.digest {
			if old.cutoff != cutoff {
				if _, err := tx.ExecContext(ctx, qMinuteCutoff, cutoff, k.minute, k.project); err != nil {
					return fmt.Errorf("writing credited minute cutoff: %w", err)
				}
			}
			continue
		}
		if err := writeMinute(ctx, tx, qMinuteUpdate, z); err != nil {
			return err
		}
		res.Updated++
	}
	return nil
}

// newCutoffs returns the cutoff exceptions of the minutes [ft,
// u.FinalBefore) as merged ranges. A minute the run rewrote has the run's
// cutoff; any other minute with rows has its rows' cutoff; any other
// minute none. Minutes finalized with minute + Lead record nothing.
func newCutoffs(have map[minuteKey]minuteState, frozen map[int64]bool, u MinuteUpdate, ft time.Time) []CutoffRange {
	stored := map[int64]int64{}
	for k, s := range have {
		stored[k.minute] = max(stored[k.minute], s.cutoff)
	}
	from, to := u.From.Unix(), u.To.Unix()
	var out []CutoffRange
	for m := ft; m.Before(u.FinalBefore); m = m.Add(time.Minute) {
		s := m.Unix()
		var c int64
		switch rows, ok := stored[s]; {
		case s >= from && s < to && !frozen[s]:
			c = u.Cutoff.Unix()
		case ok:
			c = rows
		}
		if c == m.Add(u.Lead).Unix() {
			continue
		}
		cut := time.Time{}
		if c != 0 {
			cut = time.Unix(c, 0).UTC()
		}
		if n := len(out); n > 0 && out[n-1].To.Equal(m) && out[n-1].Cutoff.Equal(cut) {
			out[n-1].To = m.Add(time.Minute)
			continue
		}
		out = append(out, CutoffRange{From: m, To: m.Add(time.Minute), Cutoff: cut})
	}
	return out
}

// addCutoff stores r, extending the last stored range when r continues it
// with the same cutoff.
func addCutoff(ctx context.Context, tx *sql.Tx, r CutoffRange) error {
	var from, to, cutoff int64
	err := tx.QueryRowContext(ctx, qCutoffLast).Scan(&from, &to, &cutoff)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading cutoffs: %w", err)
	}
	if err == nil && to == r.From.Unix() && cutoff == unixOrZero(r.Cutoff) {
		if _, err := tx.ExecContext(ctx, qCutoffExtend, r.To.Unix(), from); err != nil {
			return fmt.Errorf("extending cutoff range: %w", err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, qCutoffInsert, r.From.Unix(), r.To.Unix(), unixOrZero(r.Cutoff)); err != nil {
		return fmt.Errorf("storing cutoff range: %w", err)
	}
	return nil
}

// forgetCutoffs drops the recorded cutoffs of minutes at or after t.
func forgetCutoffs(ctx context.Context, tx *sql.Tx, t time.Time) error {
	if _, err := tx.ExecContext(ctx, qCutoffForget, t.Unix()); err != nil {
		return fmt.Errorf("forgetting cutoffs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, qCutoffClip, t.Unix(), t.Unix(), t.Unix()); err != nil {
		return fmt.Errorf("forgetting cutoffs: %w", err)
	}
	return nil
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// CutoffsBetween returns the stored cutoff exceptions overlapping [from,
// to), clipped to it, oldest first.
func (d *DB) CutoffsBetween(ctx context.Context, from, to time.Time) ([]CutoffRange, error) {
	var out []CutoffRange
	err := d.query(ctx, qCutoffRange, []any{from.Unix(), to.Unix()}, func(r *sql.Rows) error {
		var a, b, c int64
		if err := r.Scan(&a, &b, &c); err != nil {
			return err
		}
		cr := CutoffRange{From: time.Unix(max(a, from.Unix()), 0).UTC(), To: time.Unix(min(b, to.Unix()), 0).UTC()}
		if c != 0 {
			cr.Cutoff = time.Unix(c, 0).UTC()
		}
		out = append(out, cr)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading cutoffs: %w", err)
	}
	return out, nil
}

// FinalThrough returns the final watermark: every minute before it is final
// locally. It is zero before the first run.
func (d *DB) FinalThrough(ctx context.Context) (time.Time, error) {
	v, err := d.Get(ctx, KeyFinalThrough)
	if err != nil || v == "" {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading %s: %w", KeyFinalThrough, err)
	}
	return t.UTC(), nil
}

func finalThrough(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var v string
	err := tx.QueryRowContext(ctx, qKVGet, KeyFinalThrough).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading %s: %w", KeyFinalThrough, err)
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading %s: %w", KeyFinalThrough, err)
	}
	return t.UTC(), nil
}

func setFinalThrough(ctx context.Context, tx *sql.Tx, t, now time.Time) error {
	if _, err := tx.ExecContext(ctx, qKVSet, KeyFinalThrough, t.UTC().Format(time.RFC3339), now.UnixMilli()); err != nil {
		return fmt.Errorf("setting %s: %w", KeyFinalThrough, err)
	}
	return nil
}

// minuteStates reads the digests and cutoffs of the stored rows in [from, to).
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
		if err := rows.Scan(&k.minute, &k.project, &s.digest, &s.sentDigest, &s.cutoff); err != nil {
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
		args = []any{m.Minute.Unix(), m.Project, m.Label, m.Credited, cuts, flags, m.RulesVersion, m.Digest, m.Cutoff.Unix()}
	} else {
		args = []any{m.Label, m.Credited, cuts, flags, m.RulesVersion, m.Digest, m.Cutoff.Unix(), m.Minute.Unix(), m.Project}
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

// StoredMinutes returns every stored row in [from, to), with its cutoff,
// sorted by minute and project.
func (d *DB) StoredMinutes(ctx context.Context, from, to time.Time) ([]CreditedMinute, error) {
	var out []CreditedMinute
	err := d.query(ctx, qMinutesStored, []any{from.Unix(), to.Unix()}, func(r *sql.Rows) error {
		var (
			m           CreditedMinute
			at, cutoff  int64
			cuts, flags string
		)
		if err := r.Scan(&at, &m.Project, &m.Label, &m.Credited, &cuts, &flags, &m.RulesVersion, &m.Digest, &cutoff); err != nil {
			return err
		}
		m.Minute, m.Cutoff = time.Unix(at, 0).UTC(), time.Unix(cutoff, 0).UTC()
		if err := decodeCodes(&m, cuts, flags); err != nil {
			return err
		}
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading credited minutes: %w", err)
	}
	return out, nil
}

func decodeCodes(m *CreditedMinute, cuts, flags string) error {
	if err := json.Unmarshal([]byte(cuts), &m.Cuts); err != nil {
		return fmt.Errorf("decoding cuts: %w", err)
	}
	if err := json.Unmarshal([]byte(flags), &m.Flags); err != nil {
		return fmt.Errorf("decoding flags: %w", err)
	}
	return nil
}

// UnsentMinutes returns rows whose current content the server hasn't
// accepted yet, oldest minute first and, within a minute, smallest credit
// first (so a project losing credit is sent before one gaining it). Minutes
// in held are left out: they are settling and must not reach the server
// before they are final (ADR 0016). It returns whole minutes only: at most
// limit rows, unless the first minute alone has more, in which case that
// minute is returned whole. A minute's rows are never split across batches,
// because the server can keep its 60-second cap only by applying a minute's
// decreases and increases in one request. Final minutes (before
// finalBefore) whose content the server already has are first marked done.
func (d *DB) UnsentMinutes(ctx context.Context, finalBefore time.Time, held Span, limit int) ([]CreditedMinute, error) {
	if limit < 1 {
		limit = 1
	}
	if _, err := d.sql.ExecContext(ctx, qMinutesSettleFinal, finalBefore.Unix()); err != nil {
		return nil, fmt.Errorf("settling final minutes: %w", err)
	}
	hf, ht := held.From.Unix(), held.To.Unix()
	if !held.To.After(held.From) {
		hf, ht = 0, 0
	}
	var out []CreditedMinute
	err := d.query(ctx, qMinutesUnsent, []any{hf, ht, hf, ht, limit - 1}, func(r *sql.Rows) error {
		var (
			m           CreditedMinute
			at          int64
			cuts, flags string
		)
		if err := r.Scan(&at, &m.Project, &m.Label, &m.Credited, &cuts, &flags, &m.RulesVersion, &m.Digest); err != nil {
			return err
		}
		m.Minute = time.Unix(at, 0).UTC()
		if err := decodeCodes(&m, cuts, flags); err != nil {
			return err
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

// PruneCrediting deletes full heartbeats, credited minutes and cutoff
// ranges older than the retention period (90 days by default). Evidence
// days are kept (one small row per day), without the active-time records
// and cutoffs of days wholly before the cut. Repo mapping rows go once a
// newer row for the same path is older than the cut.
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
		if err := bumpGeneration(ctx, tx, r.Heartbeats, d.now()); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, qMinutesPrune, cut.Unix())
		if err != nil {
			return fmt.Errorf("pruning credited minutes: %w", err)
		}
		if r.CreditedMinutes, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("pruning credited minutes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, qCutoffPrune, cut.Unix()); err != nil {
			return fmt.Errorf("pruning cutoffs: %w", err)
		}
		// A day whose raw events are gone can't be rebuilt; its kept active
		// time and cutoffs go with them.
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
