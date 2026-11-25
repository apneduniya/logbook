package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/pkg/evidence"
)

// ErrBusy means no decode slot freed up in time (503).
var ErrBusy = errors.New("the server is busy decoding other submissions; try again in a minute")

// evidenceWait is how long a read of a part's rows waits for a decode slot.
const evidenceWait = time.Minute

// EvidenceRow is one opened row of a stored part, decoded
// (docs/api/admin.md "submissions/{id}/evidence").
type EvidenceRow struct {
	// Day is the evidence day of the row's block.
	Day string `json:"day"`
	// Bucket is the row's bucket key: 0 for the repo bucket, else 1 + the
	// Unix minute (evidence.MinuteKey).
	Bucket uint64 `json:"bucket"`
	// BucketMinute is the minute of a minute bucket; null for the repo bucket.
	BucketMinute *time.Time `json:"bucket_minute"`
	Seq          int64      `json:"seq"`
	Type         string     `json:"type"`
	Time         time.Time  `json:"time"`
	// Value is the row's value as it was hashed (the "v" of its line).
	Value json.RawMessage `json:"value"`
}

// PartRows reads one stored part and decodes its opened rows, in block,
// bucket and sequence order, returning those at [offset, offset+limit)
// and how many there are. The part is decoded within one of the shared
// decode slots, waiting up to a minute for one (ErrBusy after that), and
// dropped when it returns. It checks no caller, so its callers must.
// ErrNotFound when the submission or the device's part doesn't exist,
// ErrEvidenceDeleted once its raw rows are gone.
func (c *Checker) PartRows(ctx context.Context, id, device uuid.UUID, offset, limit int) ([]EvidenceRow, int, error) {
	wctx, cancel := context.WithTimeout(ctx, evidenceWait)
	defer cancel()
	release, err := c.slots.acquire(wctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, ErrBusy
	}
	defer release()
	data, err := c.partData(ctx, id, device)
	if err != nil {
		return nil, 0, err
	}
	part, err := decodePart(data)
	if err != nil {
		return nil, 0, fmt.Errorf("part of device %s: %w", device, err)
	}
	if c.decoded != nil {
		c.decoded(1)
		defer c.decoded(-1)
	}
	rows := []EvidenceRow{}
	n := 0
	for _, b := range part.Blocks {
		for _, o := range b.Opened {
			for _, line := range o.Lines {
				i := n
				n++
				if i < offset || i >= offset+limit {
					continue
				}
				row, err := decodeRow(b.Day, o.Key, line)
				if err != nil {
					return nil, 0, fmt.Errorf("part of device %s, block %s: %w", device, b.Day, err)
				}
				rows = append(rows, row)
			}
		}
	}
	return rows, n, nil
}

// decodeRow decodes one opened line of a bucket.
func decodeRow(day string, bucket uint64, line string) (EvidenceRow, error) {
	r, err := evidence.DecodeLine([]byte(line))
	if err != nil {
		return EvidenceRow{}, err
	}
	var l struct {
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal([]byte(line), &l); err != nil {
		return EvidenceRow{}, fmt.Errorf("reading the value of seq %d: %w", r.Seq, err)
	}
	out := EvidenceRow{Day: day, Bucket: bucket, Seq: r.Seq, Type: r.Type, Time: r.Time.UTC(), Value: l.V}
	if bucket != evidence.RepoKey {
		m := evidence.KeyMinute(bucket)
		out.BucketMinute = &m
	}
	return out, nil
}
