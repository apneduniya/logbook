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
)

// Sealing an evidence block (ADR 0019 point 4) takes three steps, each one
// transaction:
//
//  1. ReserveSeal freezes the block's upper bound, logs the repo snapshot
//     that opens the next range and reserves the day;
//  2. SaveSealStep stores each hashing step: the buckets it closed and the
//     hasher's state;
//  3. CompleteSeal stores the last buckets and the signed day, and only
//     then lets retention delete the block's rows (KeySealed).
//
// A new device starting its chain (StartChain) drops a reservation another
// device left.

// Sealing is the evidence block being sealed.
type Sealing struct {
	DeviceID      string
	Day           string
	PrevChainHash string
	// SeqFrom and SeqTo bound the block as signed: it holds the rows with
	// SeqFrom < seq <= SeqTo. SeqTo was frozen by ReserveSeal.
	SeqFrom, SeqTo int64
	// Cursor is the next bucket key to hash: evidence.RepoKey before the
	// repo bucket, then the first minute bucket key not read yet.
	Cursor uint64
	// Hasher is the saved evidence.Hasher, nil before the first step.
	Hasher []byte
	// Buckets is how many of the block's buckets are stored so far.
	Buckets    int
	ReservedAt time.Time
}

// ErrSealing means a sealing step doesn't match the stored reservation:
// none is stored, it is another block's, or another step got there first.
var ErrSealing = errors.New("evidence block being sealed has changed")

// ReserveSeal freezes the next block of a device's chain: the rows with
// seqFrom < seq <= H, where H is the log's high-water mark read in this
// transaction. If the range holds no row other than repo rows it stores
// nothing and returns ok = false: the day is a gap and the range goes on
// into the next block. Otherwise, in the same transaction, it stores the
// reservation, logs the latest mapping of every path as new repo rows
// (they take the next sequence numbers, so they open the next range, even
// where nothing changed), and writes marks: the caller's record that no
// minute of the day or earlier may be computed or stored from now on.
// It fails with ErrSealing if a block is already being sealed.
func (d *DB) ReserveSeal(ctx context.Context, deviceID, day, prevChainHash string, seqFrom int64, marks ...KV) (s Sealing, ok bool, err error) {
	now := d.now()
	err = d.inTx(ctx, func(tx *sql.Tx) error {
		w, err := openLog(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := scanSealing(tx.QueryRowContext(ctx, qSealingGet))
		if err == nil {
			return fmt.Errorf("%w: %s is being sealed", ErrSealing, cur.Day)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		high := w.seq
		var found int
		if err := tx.QueryRowContext(ctx, qHasEventRows, seqFrom, high).Scan(&found); err != nil {
			return fmt.Errorf("looking for rows to seal: %w", err)
		}
		if found == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, qSealingInsert, deviceID, day, prevChainHash, seqFrom, high, now.UnixMilli()); err != nil {
			return fmt.Errorf("reserving %s for sealing: %w", day, err)
		}
		if err := snapshotRepos(ctx, tx, w, now); err != nil {
			return err
		}
		if err := w.close(ctx, tx, now); err != nil {
			return err
		}
		if err := setMarks(ctx, tx, now, marks); err != nil {
			return err
		}
		s = Sealing{DeviceID: deviceID, Day: day, PrevChainHash: prevChainHash, SeqFrom: seqFrom, SeqTo: high,
			ReservedAt: now.UTC().Truncate(time.Millisecond)}
		ok = true
		return nil
	})
	if err != nil {
		return Sealing{}, false, err
	}
	return s, ok, nil
}

// snapshotRepos logs the mapping at the log's current high-water mark again:
// one new repo row per path, dated now, with that path's latest project
// (ADR 0019 point 4). Unlike RecordRepos it logs paths whose mapping hasn't
// changed.
func snapshotRepos(ctx context.Context, tx *sql.Tx, w *logWriter, now time.Time) error {
	type repo struct{ path, project string }
	var repos []repo
	err := queryRows(ctx, tx, qRepoSnapshotRows, []any{w.seq}, func(r *sql.Rows) error {
		var x repo
		if err := r.Scan(&x.path, &x.project); err != nil {
			return err
		}
		repos = append(repos, x)
		return nil
	})
	if err != nil {
		return fmt.Errorf("reading the repo mapping to snapshot: %w", err)
	}
	if len(repos) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, qRepoInsert)
	if err != nil {
		return fmt.Errorf("preparing: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, x := range repos {
		if err := w.insert(ctx, stmt, now.UnixMilli(), x.path, x.project); err != nil {
			return fmt.Errorf("logging the repo snapshot: %w", err)
		}
	}
	return nil
}

// Sealing returns the block being sealed; ok is false when there is none.
func (d *DB) Sealing(ctx context.Context) (s Sealing, ok bool, err error) {
	s, err = scanSealing(d.sql.QueryRowContext(ctx, qSealingGet))
	if errors.Is(err, sql.ErrNoRows) {
		return Sealing{}, false, nil
	}
	if err != nil {
		return Sealing{}, false, err
	}
	return s, true, nil
}

func scanSealing(r scanner) (Sealing, error) {
	var (
		s        Sealing
		cursor   int64
		reserved int64
	)
	err := r.Scan(&s.DeviceID, &s.Day, &s.PrevChainHash, &s.SeqFrom, &s.SeqTo, &cursor, &s.Hasher, &s.Buckets, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return Sealing{}, err
	}
	if err != nil {
		return Sealing{}, fmt.Errorf("reading the block being sealed: %w", err)
	}
	if cursor < 0 {
		return Sealing{}, fmt.Errorf("reading the block being sealed: cursor %d", cursor)
	}
	s.Cursor = uint64(cursor)
	s.ReservedAt = time.UnixMilli(reserved).UTC()
	return s, nil
}

// SaveSealStep stores one hashing step of the block being sealed: the
// buckets it closed, appended to those already stored (s.Buckets is how
// many that was when the step began), and its new cursor and hasher state.
// It fails with ErrSealing if s isn't the stored block's latest state.
func (d *DB) SaveSealStep(ctx context.Context, s Sealing, cursor uint64, hasher []byte, closed []evidence.Bucket) error {
	return d.inTx(ctx, func(tx *sql.Tx) error {
		if err := checkSealing(ctx, tx, s); err != nil {
			return err
		}
		if err := insertBuckets(ctx, tx, s, closed); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, qSealingStep, keyArg(cursor), hasher, s.Buckets+len(closed)); err != nil {
			return fmt.Errorf("saving sealing progress: %w", err)
		}
		return nil
	})
}

// CompleteSeal ends the block being sealed, in one transaction: it stores
// the buckets the last step closed, stores e (the block's signed day,
// normally pending), moves KeySealed to the block's end, so retention may
// delete its rows from now on (never before its record and bucket list are
// stored), deletes the reservation and writes marks. e.Buckets must equal
// the number of stored buckets. It fails with ErrSealing if s isn't the
// stored block's latest state, and if the device already has a day e.Day.
func (d *DB) CompleteSeal(ctx context.Context, s Sealing, closed []evidence.Bucket, e EvidenceDay, marks ...KV) error {
	counts, err := json.Marshal(e.EventCounts)
	if err != nil {
		return fmt.Errorf("encoding event counts: %w", err)
	}
	if e.DeviceID != s.DeviceID || e.Day != s.Day || e.SeqFrom != s.SeqFrom+1 || e.SeqTo != s.SeqTo ||
		e.Buckets != s.Buckets+len(closed) {
		return fmt.Errorf("%w: %s %s [%d, %d] with %d buckets doesn't complete %s %s (%d, %d] with %d",
			ErrSealing, e.DeviceID, e.Day, e.SeqFrom, e.SeqTo, e.Buckets, s.DeviceID, s.Day, s.SeqFrom, s.SeqTo,
			s.Buckets+len(closed))
	}
	now := d.now()
	return d.inTx(ctx, func(tx *sql.Tx) error {
		if err := checkSealing(ctx, tx, s); err != nil {
			return err
		}
		if err := insertBuckets(ctx, tx, s, closed); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, qEvidenceInsert, e.DeviceID, e.Day, e.PrevChainHash, e.ContentHash,
			e.ChainHash, e.Signature, e.RulesVersion, string(counts), e.SeqFrom, e.SeqTo, e.Buckets,
			nullUnix(e.TimeFrom), nullUnix(e.TimeTo), e.State, e.Error, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			return fmt.Errorf("storing evidence day %s: %w", e.Day, err)
		}
		if _, err := tx.ExecContext(ctx, qSealingDelete); err != nil {
			return fmt.Errorf("ending the sealing of %s: %w", e.Day, err)
		}
		marks = append(marks, KV{Key: KeySealed, Value: strconv.FormatInt(e.SeqTo, 10)})
		return setMarks(ctx, tx, now, marks)
	})
}

// checkSealing fails with ErrSealing unless the stored reservation is s,
// at the same step.
func checkSealing(ctx context.Context, tx *sql.Tx, s Sealing) error {
	cur, err := scanSealing(tx.QueryRowContext(ctx, qSealingGet))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no block is being sealed", ErrSealing)
	}
	if err != nil {
		return err
	}
	if cur.DeviceID != s.DeviceID || cur.Day != s.Day || cur.SeqFrom != s.SeqFrom || cur.SeqTo != s.SeqTo ||
		cur.Buckets != s.Buckets || cur.Cursor != s.Cursor {
		return fmt.Errorf("%w: sealing %s %s, step at bucket %d", ErrSealing, cur.DeviceID, cur.Day, cur.Buckets)
	}
	return nil
}

// insertBuckets stores closed as the block's buckets from index s.Buckets.
func insertBuckets(ctx context.Context, tx *sql.Tx, s Sealing, closed []evidence.Bucket) error {
	if len(closed) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, qBucketInsert)
	if err != nil {
		return fmt.Errorf("preparing: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for j, b := range closed {
		if _, err := stmt.ExecContext(ctx, s.DeviceID, s.Day, s.Buckets+j, keyArg(b.Key), b.Salt[:], b.Hash[:]); err != nil {
			return fmt.Errorf("storing evidence buckets of %s: %w", s.Day, err)
		}
	}
	return nil
}

// keyArg stores a bucket key in an INTEGER column. Keys are 1 + a Unix
// minute (or 0), far below 2^63.
func keyArg(k uint64) int64 { return int64(k) } //nolint:gosec // see above

// Buckets returns a sealed block's bucket list in key order (none for a
// block sealed before the agent kept them), or the buckets stored so far
// of the block being sealed.
func (s *Snapshot) Buckets(ctx context.Context, deviceID, day string) ([]evidence.Bucket, error) {
	var out []evidence.Bucket
	err := queryRows(ctx, s.tx, qBucketList, []any{deviceID, day}, func(r *sql.Rows) error {
		var (
			i, key     int64
			salt, hash []byte
			b          evidence.Bucket
		)
		if err := r.Scan(&i, &key, &salt, &hash); err != nil {
			return err
		}
		if i != int64(len(out)) || key < 0 || len(salt) != len(b.Salt) || len(hash) != len(b.Hash) {
			return fmt.Errorf("bucket %d of %s is malformed", i, day)
		}
		b.Key = uint64(key)
		copy(b.Salt[:], salt)
		copy(b.Hash[:], hash)
		out = append(out, b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the evidence buckets of %s: %w", day, err)
	}
	return out, nil
}

// StartChain starts the evidence chain of a device that replaced another
// (ADR 0017 point 5, ADR 0019 point 4), in one transaction: it records the
// log's high-water mark as the device's first seq_from, logs the latest
// mapping of every path as new repo rows (its first rows), drops a block
// that was being sealed with the buckets stored for it, and writes marks.
// It returns the baseline.
func (d *DB) StartChain(ctx context.Context, deviceID string, marks ...KV) (int64, error) {
	now := d.now()
	var base int64
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		w, err := openLog(ctx, tx)
		if err != nil {
			return err
		}
		base = w.seq
		if _, err := tx.ExecContext(ctx, qBaselineSet, deviceID, base); err != nil {
			return fmt.Errorf("recording where the chain starts: %w", err)
		}
		if err := snapshotRepos(ctx, tx, w, now); err != nil {
			return err
		}
		if err := w.close(ctx, tx, now); err != nil {
			return err
		}
		cur, err := scanSealing(tx.QueryRowContext(ctx, qSealingGet))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			if _, err := tx.ExecContext(ctx, qBucketsDrop, cur.DeviceID, cur.Day); err != nil {
				return fmt.Errorf("dropping the buckets of %s: %w", cur.Day, err)
			}
			if _, err := tx.ExecContext(ctx, qSealingDelete); err != nil {
				return fmt.Errorf("dropping the sealing of %s: %w", cur.Day, err)
			}
		}
		return setMarks(ctx, tx, now, marks)
	})
	return base, err
}

// ChainBaseline returns where a device's chain starts: the signed seq_from
// of its first block (0 unless StartChain recorded one).
func (d *DB) ChainBaseline(ctx context.Context, deviceID string) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(ctx, qBaselineGet, deviceID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading where the chain starts: %w", err)
	}
	return n, nil
}

// setMarks writes key-value marks inside tx.
func setMarks(ctx context.Context, tx *sql.Tx, now time.Time, marks []KV) error {
	for _, m := range marks {
		if _, err := tx.ExecContext(ctx, qKVSet, m.Key, m.Value, now.UnixMilli()); err != nil {
			return fmt.Errorf("setting %s: %w", m.Key, err)
		}
	}
	return nil
}
