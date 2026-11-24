package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/evidence"
)

// StoredDay is one stored link of a device's evidence chain.
type StoredDay struct {
	// Record holds the signed fields. Its block fields (SeqFrom, SeqTo,
	// Buckets, TimeFrom, TimeTo) are zero when HasBlock is false.
	Record      evidence.Day
	ChainHash   string
	Signature   []byte
	EventCounts map[string]int
	// HasBlock is false for a record stored before ADR 0019 (dev only),
	// which has no block fields and can't back a submission.
	HasBlock  bool
	CreatedAt time.Time
}

// Days returns the stored evidence days of the user's device whose UTC day
// is in [first, last] (dates; any time of day is ignored), oldest first.
// It is empty when the device isn't the user's or has no such days. The
// submission re-check (ADR 0020) checks proofs against Record.
func (s *Service) Days(ctx context.Context, userID, deviceID uuid.UUID, first, last time.Time) ([]StoredDay, error) {
	rows, err := s.st.EvidenceDaysOfUserDevice(ctx, db.EvidenceDaysOfUserDeviceParams{
		DeviceID: deviceID, UserID: userID,
		FirstDay: pgtype.Date{Time: utcDate(first), Valid: true},
		LastDay:  pgtype.Date{Time: utcDate(last), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("reading evidence days: %w", err)
	}
	out := make([]StoredDay, 0, len(rows))
	for _, r := range rows {
		d, err := storedDay(r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func utcDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func storedDay(r db.EvidenceDay) (StoredDay, error) {
	var counts map[string]int
	if err := json.Unmarshal(r.EventCounts, &counts); err != nil {
		return StoredDay{}, fmt.Errorf("decoding event counts of %s: %w", r.Day.Time.Format(time.DateOnly), err)
	}
	out := StoredDay{
		Record: evidence.Day{
			DeviceID: r.DeviceID.String(), Day: r.Day.Time.Format(time.DateOnly), PrevChainHash: r.PrevChainHash,
			ContentHash: r.ContentHash, RulesVersion: r.RulesVersion,
		},
		ChainHash: r.ChainHash, Signature: r.Signature, EventCounts: counts, CreatedAt: r.CreatedAt,
		// The table's check sets all three or none.
		HasBlock: r.SeqFrom.Valid && r.SeqTo.Valid && r.Buckets.Valid,
	}
	if out.HasBlock {
		out.Record.SeqFrom, out.Record.SeqTo, out.Record.Buckets = r.SeqFrom.Int64, r.SeqTo.Int64, int(r.Buckets.Int32)
		out.Record.TimeFrom, out.Record.TimeTo = utcPtr(r.TimeFrom), utcPtr(r.TimeTo)
	}
	return out, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
