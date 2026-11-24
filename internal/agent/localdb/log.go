package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// The event log (ADR 0017). Every row the struggle engine can read
// (heartbeats, AI events, activity samples, git events, Claude Code active
// time) and every repo mapping row takes the next sequence number from one
// counter inside its inserting transaction. SQLite serialises writers, so
// the committed rows are exactly the numbers 1..H still present, and a read
// transaction that reads H first sees exactly the rows at or below it: a
// write that hasn't committed is entirely outside it. Logged rows are never
// updated; only rows sealed by the device's evidence chain are deleted.

// Log state keys.
const (
	// keySeq is the last sequence number handed out (H).
	keySeq = "log.seq"
	// KeySealed is the highest sequence number sealed by the current
	// device's evidence chain: rows at or below it may be pruned or
	// trimmed, no others. InsertEvidenceDay moves it; a new device chain
	// sets it back to 0.
	KeySealed = "log.sealed"
	// KeyDroppedSamples counts activity samples dropped because the table
	// was full of rows not sealed yet.
	KeyDroppedSamples = "log.dropped_samples"
	// keyFloor is the deletion floor, unix ms: every raw row deleted by the
	// row cap or pruning had a time before it, and every row at or after it
	// is still present. A computation that reads below it can't be replayed
	// from the sealed blocks, so the crediting runner doesn't make one.
	keyFloor = "log.floor"
)

// Logged is a logged row's value with its sequence number.
type Logged[T any] struct {
	Seq   int64
	Event T
}

// Events are logged raw events, each slice ordered by event time, then
// sequence number. Times are UTC with millisecond precision (how they are
// stored); heartbeats are exactly as accepted.
type Events struct {
	Activity   []Logged[wire.ActivitySample]
	Heartbeats []Logged[wire.Heartbeat]
	AI         []Logged[wire.AIEvent]
	Git        []Logged[wire.GitEvent]
}

// logWriter hands out sequence numbers inside one write transaction.
type logWriter struct {
	seq     int64 // the last number handed out
	changed bool
}

// openLog reads the counter inside tx, which holds the write lock.
func openLog(ctx context.Context, tx *sql.Tx) (*logWriter, error) {
	n, err := txCounter(ctx, tx, keySeq)
	if err != nil {
		return nil, err
	}
	return &logWriter{seq: n}, nil
}

// insert runs a prepared logged insert whose first parameter is the row's
// sequence number. A row ignored as a duplicate takes no number.
func (w *logWriter) insert(ctx context.Context, stmt *sql.Stmt, args ...any) error {
	res, err := stmt.ExecContext(ctx, append([]any{w.seq + 1}, args...)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		w.seq++
		w.changed = true
	}
	return nil
}

// close stores the counter, in the same transaction.
func (w *logWriter) close(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if !w.changed {
		return nil
	}
	if _, err := tx.ExecContext(ctx, qKVSet, keySeq, strconv.FormatInt(w.seq, 10), now.UnixMilli()); err != nil {
		return fmt.Errorf("advancing the log: %w", err)
	}
	return nil
}

// txCounter reads an integer kv value inside tx (0 if unset).
func txCounter(ctx context.Context, tx *sql.Tx, key string) (int64, error) {
	var v string
	err := tx.QueryRowContext(ctx, qKVGet, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", key, err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is not a number: %w", key, err)
	}
	return n, nil
}

// Snapshot is a consistent view of the log: one read transaction. H and
// Generation are read first, so every read through the snapshot sees the
// rows committed by then and no others.
type Snapshot struct {
	tx *sql.Tx
	// H is the log's high-water mark: every row in the snapshot has a
	// sequence number at or below it.
	H int64
	// Generation is the deletion generation (EventGeneration) in the
	// snapshot.
	Generation int64
	// Floor is the deletion floor (keyFloor) in the same snapshot: rows with
	// a time at or after it are all present. The zero Unix time when nothing
	// was ever deleted.
	Floor time.Time
}

// View runs fn with a snapshot of the log. fn must not keep the snapshot.
func (d *DB) View(ctx context.Context, fn func(*Snapshot) error) error {
	// A read-only transaction begins deferred (not "immediate"): it takes
	// no write lock, and its snapshot starts with its first read.
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("beginning a log snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep
	s := &Snapshot{tx: tx}
	if s.H, err = txCounter(ctx, tx, keySeq); err != nil {
		return err
	}
	if s.Generation, err = txCounter(ctx, tx, keyGeneration); err != nil {
		return err
	}
	floor, err := txCounter(ctx, tx, keyFloor)
	if err != nil {
		return err
	}
	s.Floor = time.UnixMilli(floor).UTC()
	return fn(s)
}

// Window is the log for one time range at one high-water mark.
type Window struct {
	// H is the high-water mark the rows were read at.
	H int64
	// Generation is the deletion generation in the same snapshot.
	Generation int64
	// Events are the raw events with time in the range and seq <= H.
	Events Events
	// Repos is the repo mapping at H: each path's latest row.
	Repos []struggle.Repo
}

// ReadWindow returns, from one read transaction, the raw events with
// from <= time < to and the repo mapping, both at upto, or at the current
// high-water mark when upto is 0 or past it.
func (d *DB) ReadWindow(ctx context.Context, from, to time.Time, upto int64) (Window, error) {
	var w Window
	err := d.View(ctx, func(s *Snapshot) error {
		h := s.H
		if upto > 0 && upto < h {
			h = upto
		}
		ev, err := s.Events(ctx, from, to, 0, h)
		if err != nil {
			return err
		}
		repos, err := s.Repos(ctx, h)
		if err != nil {
			return err
		}
		w = Window{H: h, Generation: s.Generation, Events: ev, Repos: repos}
		return nil
	})
	return w, err
}

// Events returns the raw events with from <= time < to and a sequence
// number in (after, upto]. With after > 0 it walks the sequence range (the
// few rows logged since a previous read); otherwise the time range.
// Heartbeats that can no longer be decoded are skipped (they can't have
// come from Enqueue).
func (s *Snapshot) Events(ctx context.Context, from, to time.Time, after, upto int64) (Events, error) {
	q := eventQueries{qHeartbeatsByTime, qAIByTime, qActivityByTime, qGitByTime}
	if after > 0 {
		q = eventQueries{qHeartbeatsBySeq, qAIBySeq, qActivityBySeq, qGitBySeq}
	}
	args := []any{from.UnixMilli(), to.UnixMilli(), after, upto}
	var ev Events
	err := queryRows(ctx, s.tx, q.heartbeats, args, func(r *sql.Rows) error {
		h, seq, ok, err := scanHeartbeat(r)
		if ok {
			ev.Heartbeats = append(ev.Heartbeats, Logged[wire.Heartbeat]{Seq: seq, Event: h})
		}
		return err
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading heartbeats: %w", err)
	}
	err = queryRows(ctx, s.tx, q.ai, args, func(r *sql.Rows) error {
		e, seq, err := scanAI(r)
		ev.AI = append(ev.AI, Logged[wire.AIEvent]{Seq: seq, Event: e})
		return err
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading ai events: %w", err)
	}
	err = queryRows(ctx, s.tx, q.activity, args, func(r *sql.Rows) error {
		e, seq, err := scanActivity(r)
		ev.Activity = append(ev.Activity, Logged[wire.ActivitySample]{Seq: seq, Event: e})
		return err
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading activity samples: %w", err)
	}
	err = queryRows(ctx, s.tx, q.git, args, func(r *sql.Rows) error {
		e, seq, err := scanGit(r)
		ev.Git = append(ev.Git, Logged[wire.GitEvent]{Seq: seq, Event: e})
		return err
	})
	if err != nil {
		return Events{}, fmt.Errorf("reading git events: %w", err)
	}
	return ev, nil
}

// eventQueries is one read query per raw event table.
type eventQueries struct{ heartbeats, ai, activity, git string }

// Repos returns the repo mapping at upto: each path's row with the highest
// sequence number at or below it, sorted by path.
func (s *Snapshot) Repos(ctx context.Context, upto int64) ([]struggle.Repo, error) {
	var out []struggle.Repo
	err := queryRows(ctx, s.tx, qReposAt, []any{upto}, func(r *sql.Rows) error {
		var repo struggle.Repo
		if err := r.Scan(&repo.Path, &repo.Project); err != nil {
			return err
		}
		out = append(out, repo)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading repo mapping: %w", err)
	}
	return out, nil
}

// Rows returns every logged row with a sequence number in (after, upto] as
// the evidence encoding covers it (ADR 0017), in no particular order.
// Active time is the stored point without derived fields.
func (s *Snapshot) Rows(ctx context.Context, after, upto int64) ([]evidence.Row, error) {
	return s.readRows(ctx, rowsBySeq, []any{after, upto})
}

// RepoRows returns the repo mapping rows with a sequence number in
// (after, upto]: the rows of that range's repo bucket (ADR 0019).
func (s *Snapshot) RepoRows(ctx context.Context, after, upto int64) ([]evidence.Row, error) {
	return s.readRows(ctx, rowQueries{repo: qRepoRows}, []any{after, upto})
}

// MinuteRows returns every row with a sequence number in (after, upto]
// whose minute bucket (evidence.BucketKey) is a minute of [from, to): all
// rows but repo rows, by their evidence time (active time by its end).
// from and to should be whole minutes.
func (s *Snapshot) MinuteRows(ctx context.Context, after, upto int64, from, to time.Time) ([]evidence.Row, error) {
	return s.readRows(ctx, rowsByTime, []any{after, upto, from.UnixMilli(), to.UnixMilli()})
}

// NextMinuteRow returns the evidence time, truncated to the minute, of the
// earliest row with a sequence number in (after, upto] and a time at or
// after from, not counting repo rows. ok is false when there is none.
func (s *Snapshot) NextMinuteRow(ctx context.Context, after, upto int64, from time.Time) (next time.Time, ok bool, err error) {
	var ms sql.NullInt64
	if err := s.tx.QueryRowContext(ctx, qNextRowTime, after, upto, from.UnixMilli()).Scan(&ms); err != nil {
		return time.Time{}, false, fmt.Errorf("finding the next logged minute: %w", err)
	}
	if !ms.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(ms.Int64).UTC().Truncate(time.Minute), true, nil
}

// rowQueries is one evidence row query per logged table; "" skips a table.
type rowQueries struct{ heartbeats, ai, activity, git, active, repo string }

var (
	rowsBySeq  = rowQueries{qHeartbeatRows, qAIRows, qActivityRows, qGitRows, qActiveRows, qRepoRows}
	rowsByTime = rowQueries{qHeartbeatRowsIn, qAIRowsIn, qActivityRowsIn, qGitRowsIn, qActiveRowsIn, ""}
)

// readRows runs q's queries with args and returns their rows as evidence
// rows. Heartbeats that no longer decode are skipped (they can't have come
// from Enqueue).
func (s *Snapshot) readRows(ctx context.Context, q rowQueries, args []any) ([]evidence.Row, error) {
	var out []evidence.Row
	reads := []struct {
		name, query string
		scan        func(*sql.Rows) error
	}{
		{"heartbeats", q.heartbeats, func(r *sql.Rows) error {
			h, seq, ok, err := scanHeartbeat(r)
			if ok {
				out = append(out, evidence.HeartbeatRow(seq, h))
			}
			return err
		}},
		{"ai events", q.ai, func(r *sql.Rows) error {
			e, seq, err := scanAI(r)
			out = append(out, evidence.AIRow(seq, e))
			return err
		}},
		{"activity samples", q.activity, func(r *sql.Rows) error {
			e, seq, err := scanActivity(r)
			out = append(out, evidence.ActivityRow(seq, e))
			return err
		}},
		{"git events", q.git, func(r *sql.Rows) error {
			e, seq, err := scanGit(r)
			out = append(out, evidence.GitRow(seq, e))
			return err
		}},
		{"active time", q.active, func(r *sql.Rows) error {
			var (
				seq, from, to int64
				typ           string
				secs          float64
				a             = evidence.ActiveTime{Harness: wire.HarnessClaudeCode}
			)
			if err := r.Scan(&seq, &from, &to, &a.SessionID, &typ, &secs); err != nil {
				return err
			}
			a.From, a.To = time.UnixMilli(from).UTC(), time.UnixMilli(to).UTC()
			if typ == ActiveUser {
				a.UserSecs = secs
			} else {
				a.CLISecs = secs
			}
			out = append(out, evidence.ActiveTimeRow(seq, a))
			return nil
		}},
		{"repo mapping", q.repo, func(r *sql.Rows) error {
			var (
				seq, since int64
				repo       evidence.Repo
			)
			if err := r.Scan(&seq, &since, &repo.Path, &repo.Project); err != nil {
				return err
			}
			out = append(out, evidence.RepoRow(seq, time.UnixMilli(since), repo))
			return nil
		}},
	}
	for _, rd := range reads {
		if rd.query == "" {
			continue
		}
		if err := queryRows(ctx, s.tx, rd.query, args, rd.scan); err != nil {
			return nil, fmt.Errorf("reading logged %s: %w", rd.name, err)
		}
	}
	return out, nil
}

// OldestAbove returns the smallest sequence number still stored above
// after, or 0 when there is none.
func (s *Snapshot) OldestAbove(ctx context.Context, after int64) (int64, error) {
	var n sql.NullInt64
	if err := s.tx.QueryRowContext(ctx, qOldestSeq, after).Scan(&n); err != nil {
		return 0, fmt.Errorf("reading the oldest logged row: %w", err)
	}
	return n.Int64, nil
}

// LateResult counts logged raw events whose minute was already final.
type LateResult struct {
	Count int64
	// Latest is the latest event time among them (zero if Count is 0).
	Latest time.Time
}

// Late counts the raw events logged in (after, upto] whose own minute is
// final: they can never change that minute's credit (ADR 0017).
func (s *Snapshot) Late(ctx context.Context, after, upto int64) (LateResult, error) {
	var res LateResult
	for _, q := range []string{qLateHeartbeats, qLateAI, qLateActivity, qLateGit} {
		var n, latest int64
		if err := s.tx.QueryRowContext(ctx, q, after, upto).Scan(&n, &latest); err != nil {
			return LateResult{}, fmt.Errorf("counting late events: %w", err)
		}
		res.Count += n
		if t := time.UnixMilli(latest).UTC(); n > 0 && t.After(res.Latest) {
			res.Latest = t
		}
	}
	return res, nil
}

// scanHeartbeat reads (seq, payload); ok is false for a payload that no
// longer decodes.
func scanHeartbeat(r *sql.Rows) (h wire.Heartbeat, seq int64, ok bool, err error) {
	var payload []byte
	if err := r.Scan(&seq, &payload); err != nil {
		return wire.Heartbeat{}, 0, false, err
	}
	if json.Unmarshal(payload, &h) != nil {
		return wire.Heartbeat{}, 0, false, nil
	}
	return h, seq, true, nil
}

func scanAI(r *sql.Rows) (wire.AIEvent, int64, error) {
	var (
		e                 wire.AIEvent
		seq, at           int64
		harness, kindName string
	)
	if err := r.Scan(&seq, &at, &harness, &kindName, &e.SessionID, &e.Project, &e.Cwd, &e.Model, &e.Tool,
		&e.PromptChars, &e.Decision, &e.PromptHash); err != nil {
		return wire.AIEvent{}, 0, err
	}
	e.Time, e.Harness, e.Kind = time.UnixMilli(at).UTC(), wire.Harness(harness), wire.AIEventKind(kindName)
	return e, seq, nil
}

func scanActivity(r *sql.Rows) (wire.ActivitySample, int64, error) {
	var (
		s       wire.ActivitySample
		seq, at int64
	)
	if err := r.Scan(&seq, &at, &s.IdleSeconds, &s.FocusApp, &s.FocusKind); err != nil {
		return wire.ActivitySample{}, 0, err
	}
	s.Time = time.UnixMilli(at).UTC()
	return s, seq, nil
}

func scanGit(r *sql.Rows) (wire.GitEvent, int64, error) {
	var (
		g       wire.GitEvent
		seq, at int64
	)
	if err := r.Scan(&seq, &at, &g.Project, &g.Kind, &g.CommitHash, &g.FilesChanged, &g.Insertions, &g.Deletions); err != nil {
		return wire.GitEvent{}, 0, err
	}
	g.Time = time.UnixMilli(at).UTC()
	return g, seq, nil
}

// sealedSeq reads KeySealed inside tx.
func sealedSeq(ctx context.Context, tx *sql.Tx) (int64, error) {
	return txCounter(ctx, tx, KeySealed)
}

// CapHorizon is how old a sealed row must be before the row cap may delete
// it. Open minutes are computed from the last two hours of events (the
// window plus struggle.ContextBefore), so rows they read are never trimmed
// while the clock runs forward; if it is set back below the deletion floor,
// the runner waits for it instead (Snapshot.Floor).
const CapHorizon = 3 * time.Hour

// trimSealed runs a cap query (limit, sealed, horizon) and returns how many
// rows it deleted: the oldest rows beyond limit, if the evidence chain
// sealed them and their time is before horizon. It raises the deletion
// floor to horizon when it deletes anything.
func trimSealed(ctx context.Context, tx *sql.Tx, query string, limit, sealed int64, horizon time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, query, limit, sealed, horizon.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("trimming: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("trimming: %w", err)
	}
	if err := raiseFloor(ctx, tx, n, horizon); err != nil {
		return 0, err
	}
	return n, nil
}

// raiseFloor moves the deletion floor up to t after deleting rows that were
// all older than t. The floor never moves down.
func raiseFloor(ctx context.Context, tx *sql.Tx, deleted int64, t time.Time) error {
	if deleted <= 0 {
		return nil
	}
	cur, err := txCounter(ctx, tx, keyFloor)
	if err != nil {
		return err
	}
	if t.UnixMilli() <= cur {
		return nil
	}
	if _, err := tx.ExecContext(ctx, qKVSet, keyFloor, strconv.FormatInt(t.UnixMilli(), 10), t.UnixMilli()); err != nil {
		return fmt.Errorf("raising the deletion floor: %w", err)
	}
	return nil
}
