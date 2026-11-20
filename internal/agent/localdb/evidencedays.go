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

// EvidenceDay is one sealed link of a device's evidence chain.
type EvidenceDay struct {
	DeviceID      string
	Day           string
	PrevChainHash string
	ContentHash   string
	ChainHash     string
	Signature     []byte
	RulesVersion  string
	EventCounts   map[string]int
	// HighWater pins exactly which rows of the raw event tables (heartbeats,
	// AI events, activity samples, git events) the content hash covers.
	HighWater HighWater
	// ActiveTime is exactly the active-time records the content hash
	// covers, kept because active time can't be pinned by row ids. nil
	// means they are not kept: the day was sealed before they were, or its
	// raw events have been pruned. Sealing stores a non-nil slice.
	ActiveTime []evidence.ActiveTime
	// Cutoffs are the day's final minutes whose arrival cutoff isn't the
	// default (minute + the lead), as stored when the day was sealed, so a
	// replay of the day's credit uses exactly the agent's cutoffs (ADR
	// 0016). nil means not kept: the day was sealed before they were, or
	// its raw events have been pruned. Sealing stores a non-nil slice.
	Cutoffs   []CutoffRange
	State     string
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InsertEvidenceDay stores a newly sealed day. It fails if the device
// already has a row for that day.
func (d *DB) InsertEvidenceDay(ctx context.Context, e EvidenceDay) error {
	counts, err := json.Marshal(e.EventCounts)
	if err != nil {
		return fmt.Errorf("encoding event counts: %w", err)
	}
	active := e.ActiveTime
	if active == nil {
		active = []evidence.ActiveTime{}
	}
	at, err := json.Marshal(active)
	if err != nil {
		return fmt.Errorf("encoding active time: %w", err)
	}
	cutoffs := e.Cutoffs
	if cutoffs == nil {
		cutoffs = []CutoffRange{}
	}
	cs, err := json.Marshal(cutoffs)
	if err != nil {
		return fmt.Errorf("encoding cutoffs: %w", err)
	}
	now := d.now().UnixMilli()
	_, err = d.sql.ExecContext(ctx, qEvidenceInsert, e.DeviceID, e.Day, e.PrevChainHash, e.ContentHash,
		e.ChainHash, e.Signature, e.RulesVersion, string(counts), e.HighWater.Heartbeats, e.HighWater.AI,
		e.HighWater.Activity, e.HighWater.Git, string(at), string(cs), e.State, e.Error, now, now)
	if err != nil {
		return fmt.Errorf("storing evidence day %s: %w", e.Day, err)
	}
	return nil
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
		active, cutoffs  sql.NullString
		created, updated int64
		hw               HighWater
	)
	err = d.sql.QueryRowContext(ctx, q, args...).Scan(&e.DeviceID, &e.Day, &e.PrevChainHash,
		&e.ContentHash, &e.ChainHash, &e.Signature, &e.RulesVersion, &counts, &hw.Heartbeats, &hw.AI,
		&hw.Activity, &hw.Git, &active, &cutoffs, &e.State, &e.Error, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceDay{}, false, nil
	}
	if err != nil {
		return EvidenceDay{}, false, fmt.Errorf("reading evidence chain: %w", err)
	}
	if err := json.Unmarshal([]byte(counts), &e.EventCounts); err != nil {
		return EvidenceDay{}, false, fmt.Errorf("decoding event counts: %w", err)
	}
	if active.Valid {
		e.ActiveTime = []evidence.ActiveTime{}
		if err := json.Unmarshal([]byte(active.String), &e.ActiveTime); err != nil {
			return EvidenceDay{}, false, fmt.Errorf("decoding active time of %s: %w", e.Day, err)
		}
	}
	if cutoffs.Valid {
		e.Cutoffs = []CutoffRange{}
		if err := json.Unmarshal([]byte(cutoffs.String), &e.Cutoffs); err != nil {
			return EvidenceDay{}, false, fmt.Errorf("decoding cutoffs of %s: %w", e.Day, err)
		}
	}
	e.HighWater = hw
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
