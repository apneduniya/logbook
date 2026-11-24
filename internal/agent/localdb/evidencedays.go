package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
)

// Evidence day states.
const (
	// EvidencePending: sealed and signed, not yet accepted by the server.
	EvidencePending = "pending"
	// EvidenceAccepted: stored by the server; the chain continues from it.
	EvidenceAccepted = "accepted"
	// EvidenceConflict: the server holds a different value for the day or
	// expects another previous hash. The chain stops here.
	EvidenceConflict = "conflict"
	// EvidenceRejected: the server refused the day as invalid. The chain
	// stops here.
	EvidenceRejected = "rejected"
)

// EvidenceDay is one sealed link of a device's evidence chain: one block of
// the event log per UTC day (ADR 0017, ADR 0019).
type EvidenceDay struct {
	DeviceID      string
	Day           string
	PrevChainHash string
	ContentHash   string
	ChainHash     string
	Signature     []byte
	RulesVersion  string
	EventCounts   map[string]int
	// SeqFrom and SeqTo bound the block: it holds every logged row with a
	// sequence number in [SeqFrom, SeqTo]. The signed seq_from is SeqFrom -
	// 1 (Record). SeqTo is the log's high-water mark when the block's bound
	// was frozen.
	SeqFrom, SeqTo int64
	// Buckets is the block's leaf count and TimeFrom and TimeTo the minutes
	// of its first and last minute bucket (nil when it has only its repo
	// bucket), as signed. Buckets is 0 for a block sealed before the agent
	// kept bucket lists (local migration 0008): it can't be re-hashed or
	// proven.
	Buckets          int
	TimeFrom, TimeTo *time.Time
	// Legacy days were sealed by event time before the event log (local
	// migration 0007). They have no range and can't be replayed.
	Legacy    bool
	State     string
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Record returns the record the block was signed with (ADR 0019 point 2).
// It means nothing for a legacy day.
func (e EvidenceDay) Record() evidence.Day {
	return evidence.Day{DeviceID: e.DeviceID, Day: e.Day, PrevChainHash: e.PrevChainHash, ContentHash: e.ContentHash,
		RulesVersion: e.RulesVersion, SeqFrom: e.SeqFrom - 1, SeqTo: e.SeqTo, Buckets: e.Buckets,
		TimeFrom: e.TimeFrom, TimeTo: e.TimeTo}
}

// Provable reports whether the agent keeps the block's bucket list, so it
// can re-hash it and build proofs from it.
func (e EvidenceDay) Provable() bool { return !e.Legacy && e.Buckets > 0 }

// LastEvidenceDay returns the device's latest evidence day in any state.
// ok is false when the device has none.
func (d *DB) LastEvidenceDay(ctx context.Context, deviceID string) (e EvidenceDay, ok bool, err error) {
	return d.evidenceDay(ctx, qEvidenceLast, deviceID)
}

// EvidenceDayOf returns the device's evidence day for day (YYYY-MM-DD).
// ok is false when there is none.
func (d *DB) EvidenceDayOf(ctx context.Context, deviceID, day string) (e EvidenceDay, ok bool, err error) {
	return d.evidenceDay(ctx, qEvidenceDay, deviceID, day)
}

func (d *DB) evidenceDay(ctx context.Context, q string, args ...any) (e EvidenceDay, ok bool, err error) {
	e, err = scanEvidenceDay(d.sql.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceDay{}, false, nil
	}
	if err != nil {
		return EvidenceDay{}, false, err
	}
	return e, true, nil
}

// EvidenceDays returns the device's evidence days in any state, oldest
// first.
func (s *Snapshot) EvidenceDays(ctx context.Context, deviceID string) ([]EvidenceDay, error) {
	var out []EvidenceDay
	err := queryRows(ctx, s.tx, qEvidenceAll, []any{deviceID}, func(r *sql.Rows) error {
		e, err := scanEvidenceDay(r)
		out = append(out, e)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reading evidence chain: %w", err)
	}
	return out, nil
}

// scanner is *sql.Row or *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// scanEvidenceDay reads one row of qEvidenceColumns.
func scanEvidenceDay(r scanner) (EvidenceDay, error) {
	var (
		e                EvidenceDay
		counts           string
		legacy           int
		from, to         sql.NullInt64
		created, updated int64
	)
	err := r.Scan(&e.DeviceID, &e.Day, &e.PrevChainHash, &e.ContentHash, &e.ChainHash, &e.Signature,
		&e.RulesVersion, &counts, &e.SeqFrom, &e.SeqTo, &e.Buckets, &from, &to, &legacy, &e.State, &e.Error,
		&created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceDay{}, err
	}
	if err != nil {
		return EvidenceDay{}, fmt.Errorf("reading evidence chain: %w", err)
	}
	if err := json.Unmarshal([]byte(counts), &e.EventCounts); err != nil {
		return EvidenceDay{}, fmt.Errorf("decoding event counts: %w", err)
	}
	e.TimeFrom, e.TimeTo = unixPtr(from), unixPtr(to)
	e.Legacy = legacy == 1
	e.CreatedAt, e.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return e, nil
}

// unixPtr is the UTC time of Unix seconds v, or nil when v is NULL.
func unixPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

// nullUnix is t as Unix seconds, or NULL when t is nil.
func nullUnix(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

// SetEvidenceState records the server's answer for one day, together with
// any state marks, in one transaction.
func (d *DB) SetEvidenceState(ctx context.Context, deviceID, day, state, errMsg string, marks ...KV) error {
	now := d.now().UnixMilli()
	return d.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, qEvidenceSetState, state, errMsg, now, deviceID, day)
		if err != nil {
			return fmt.Errorf("updating evidence day %s: %w", day, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("updating evidence day %s: %w", day, err)
		}
		if n != 1 {
			return fmt.Errorf("updating evidence day %s: no such day", day)
		}
		for _, m := range marks {
			if _, err := tx.ExecContext(ctx, qKVSet, m.Key, m.Value, now); err != nil {
				return fmt.Errorf("setting %s: %w", m.Key, err)
			}
		}
		return nil
	})
}
