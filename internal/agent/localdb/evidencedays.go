package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
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
// the event log per UTC day (ADR 0017).
type EvidenceDay struct {
	DeviceID      string
	Day           string
	PrevChainHash string
	ContentHash   string
	ChainHash     string
	Signature     []byte
	RulesVersion  string
	EventCounts   map[string]int
	// SeqFrom and SeqTo bound the block: the content hash covers every
	// logged row with a sequence number in [SeqFrom, SeqTo]. SeqTo is the
	// log's high-water mark read in the transaction that sealed the block.
	SeqFrom, SeqTo int64
	// Legacy days were sealed by event time before the event log (local
	// migration 0007). They have no range and can't be replayed.
	Legacy    bool
	State     string
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InsertEvidenceDay stores a newly sealed day. It fails if the device
// already has a row for that day. In the same transaction it moves
// KeySealed to the block's end: its rows are sealed by the device's chain,
// so retention may delete them once they are old enough.
func (d *DB) InsertEvidenceDay(ctx context.Context, e EvidenceDay) error {
	counts, err := json.Marshal(e.EventCounts)
	if err != nil {
		return fmt.Errorf("encoding event counts: %w", err)
	}
	now := d.now().UnixMilli()
	legacy := 0
	if e.Legacy {
		legacy = 1
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, qEvidenceInsert, e.DeviceID, e.Day, e.PrevChainHash, e.ContentHash,
			e.ChainHash, e.Signature, e.RulesVersion, string(counts), e.SeqFrom, e.SeqTo, legacy, e.State, e.Error,
			now, now)
		if err != nil {
			return fmt.Errorf("storing evidence day %s: %w", e.Day, err)
		}
		if e.Legacy {
			return nil
		}
		if _, err := tx.ExecContext(ctx, qKVSet, KeySealed, strconv.FormatInt(e.SeqTo, 10), now); err != nil {
			return fmt.Errorf("recording sealed rows: %w", err)
		}
		return nil
	})
}

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
	var (
		counts           string
		legacy           int
		created, updated int64
	)
	err = d.sql.QueryRowContext(ctx, q, args...).Scan(&e.DeviceID, &e.Day, &e.PrevChainHash,
		&e.ContentHash, &e.ChainHash, &e.Signature, &e.RulesVersion, &counts, &e.SeqFrom, &e.SeqTo, &legacy,
		&e.State, &e.Error, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceDay{}, false, nil
	}
	if err != nil {
		return EvidenceDay{}, false, fmt.Errorf("reading evidence chain: %w", err)
	}
	if err := json.Unmarshal([]byte(counts), &e.EventCounts); err != nil {
		return EvidenceDay{}, false, fmt.Errorf("decoding event counts: %w", err)
	}
	e.Legacy = legacy == 1
	e.CreatedAt, e.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return e, true, nil
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
