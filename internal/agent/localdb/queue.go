package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// Queue names one of the two upload queues.
type Queue string

// The upload queues.
const (
	// HeartbeatQueue holds heartbeats waiting for the Logbook server.
	HeartbeatQueue Queue = "heartbeat_queue"
	// MirrorQueue holds heartbeats waiting to be mirrored to Hackatime.
	MirrorQueue Queue = "mirror_queue"
)

type queueSQL struct{ insert, peek, del, count, trim, oldest string }

func (q Queue) sql() (queueSQL, error) {
	switch q {
	case HeartbeatQueue:
		return queueSQL{qHBInsert, qHBPeek, qHBDelete, qHBCount, qHBTrim, qHBOldest}, nil
	case MirrorQueue:
		return queueSQL{qMirInsert, qMirPeek, qMirDelete, qMirCount, qMirTrim, qMirOldest}, nil
	}
	return queueSQL{}, fmt.Errorf("unknown queue %q", q)
}

// DroppedKey is the kv counter of rows a queue dropped because it was full.
func (q Queue) DroppedKey() string { return "dropped." + string(q) }

// Item is one queued heartbeat.
type Item struct {
	ID        int64
	Heartbeat wire.Heartbeat
}

// Enqueue stores heartbeats, in one transaction, in the Logbook queue, in
// the mirror queue too when mirror is true, in the local heartbeat log, and
// in full in the heartbeats table (local evidence, deduplicated).
// When a queue exceeds its cap the oldest rows are dropped and counted.
func (d *DB) Enqueue(ctx context.Context, hbs []wire.Heartbeat, mirror bool) error {
	if len(hbs) == 0 {
		return nil
	}
	queues := []Queue{HeartbeatQueue}
	if mirror {
		queues = append(queues, MirrorQueue)
	}
	now := d.now().UnixMilli()
	payloads := make([][]byte, len(hbs))
	for i, h := range hbs {
		b, err := json.Marshal(h)
		if err != nil {
			return fmt.Errorf("encoding heartbeat: %w", err)
		}
		payloads[i] = b
	}
	dropped := map[Queue]int64{}
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		for _, q := range queues {
			s, err := q.sql()
			if err != nil {
				return err
			}
			if err := execEach(ctx, tx, s.insert, len(payloads), func(i int) []any {
				return []any{now, payloads[i]}
			}); err != nil {
				return fmt.Errorf("enqueueing to %s: %w", q, err)
			}
			n, err := trim(ctx, tx, s.trim, d.queueCap)
			if err != nil {
				return fmt.Errorf("capping %s: %w", q, err)
			}
			if n > 0 {
				dropped[q] = n
				if err := kvAdd(ctx, tx, q.DroppedKey(), n, now); err != nil {
					return err
				}
			}
		}
		if err := execEach(ctx, tx, qLogInsert, len(hbs), func(i int) []any {
			return []any{hbs[i].Timestamp().UnixMilli(), hbs[i].Project, entityDir(hbs[i])}
		}); err != nil {
			return fmt.Errorf("logging heartbeats: %w", err)
		}
		if _, err := trim(ctx, tx, qLogTrim, d.queueCap); err != nil {
			return err
		}
		return d.logHeartbeats(ctx, tx, hbs, payloads)
	})
	if err != nil {
		return err
	}
	for q, n := range dropped {
		d.log.WarnContext(ctx, "upload queue full; dropped oldest heartbeats", "queue", string(q), "dropped", n)
	}
	return nil
}

// logHeartbeats keeps full heartbeats in the event log, inside Enqueue's
// transaction: each new one takes the next sequence number; one wakatime-cli
// resent is already stored and takes none (ADR 0017). Heartbeats are always
// kept: over the row cap only rows the evidence chain sealed are trimmed.
func (d *DB) logHeartbeats(ctx context.Context, tx *sql.Tx, hbs []wire.Heartbeat, payloads [][]byte) error {
	now := d.now()
	w, err := openLog(ctx, tx)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, qHeartbeatInsert)
	if err != nil {
		return fmt.Errorf("preparing: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for i, h := range hbs {
		if err := w.insert(ctx, stmt, h.Timestamp().UnixMilli(), h.FieldsHash(), payloads[i]); err != nil {
			return fmt.Errorf("keeping heartbeats: %w", err)
		}
	}
	if err := w.close(ctx, tx, now); err != nil {
		return err
	}
	sealed, err := sealedSeq(ctx, tx)
	if err != nil {
		return err
	}
	n, err := trimSealed(ctx, tx, qHeartbeatTrim, d.eventCap, sealed, d.now().Add(-CapHorizon))
	if err != nil {
		return err
	}
	return bumpGeneration(ctx, tx, n, now)
}

// entityDir returns the directory of a file heartbeat's entity, or "" for
// other entity types. Only the directory is kept locally, for repo discovery.
func entityDir(h wire.Heartbeat) string {
	if h.Type != "" && h.Type != "file" {
		return ""
	}
	if !filepath.IsAbs(h.Entity) {
		return ""
	}
	return filepath.Dir(h.Entity)
}

// Peek returns up to n of the oldest items in q without removing them.
// Rows whose payload can no longer be decoded are deleted and skipped.
func (d *DB) Peek(ctx context.Context, q Queue, n int) ([]Item, error) {
	s, err := q.sql()
	if err != nil {
		return nil, err
	}
	rows, err := d.sql.QueryContext(ctx, s.peek, n)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", q, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		items []Item
		bad   []int64
	)
	for rows.Next() {
		var (
			id      int64
			payload []byte
		)
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("scanning %s: %w", q, err)
		}
		var h wire.Heartbeat
		if err := json.Unmarshal(payload, &h); err != nil {
			bad = append(bad, id)
			continue
		}
		items = append(items, Item{ID: id, Heartbeat: h})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", q, err)
	}
	_ = rows.Close() // release the connection before deleting
	if len(bad) > 0 {
		d.log.WarnContext(ctx, "dropping undecodable queue rows", "queue", string(q), "count", len(bad))
		if err := d.Delete(ctx, q, bad); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// Delete removes items from q by id, in one transaction.
func (d *DB) Delete(ctx context.Context, q Queue, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	s, err := q.sql()
	if err != nil {
		return err
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		if err := execEach(ctx, tx, s.del, len(ids), func(i int) []any { return []any{ids[i]} }); err != nil {
			return fmt.Errorf("deleting from %s: %w", q, err)
		}
		return nil
	})
}

// QueueStats summarises one queue.
type QueueStats struct {
	Pending int64 `json:"pending"`
	// Oldest is when the oldest pending item was queued (zero if empty).
	Oldest time.Time `json:"oldest,omitzero"`
	// Dropped counts items ever dropped because the queue was full.
	Dropped int64 `json:"dropped"`
}

// Pending returns the number of items in q.
func (d *DB) Pending(ctx context.Context, q Queue) (int64, error) {
	s, err := q.sql()
	if err != nil {
		return 0, err
	}
	var n int64
	if err := d.sql.QueryRowContext(ctx, s.count).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting %s: %w", q, err)
	}
	return n, nil
}

// Stats returns q's size, age and drop counter.
func (d *DB) Stats(ctx context.Context, q Queue) (QueueStats, error) {
	s, err := q.sql()
	if err != nil {
		return QueueStats{}, err
	}
	var st QueueStats
	if st.Pending, err = d.Pending(ctx, q); err != nil {
		return QueueStats{}, err
	}
	var oldest sql.NullInt64
	if err := d.sql.QueryRowContext(ctx, s.oldest).Scan(&oldest); err != nil {
		return QueueStats{}, fmt.Errorf("reading %s age: %w", q, err)
	}
	if oldest.Valid {
		st.Oldest = time.UnixMilli(oldest.Int64).UTC()
	}
	if st.Dropped, err = d.Counter(ctx, q.DroppedKey()); err != nil {
		return QueueStats{}, err
	}
	return st, nil
}

// execEach runs one prepared statement n times inside tx.
func execEach(ctx context.Context, tx *sql.Tx, query string, n int, args func(i int) []any) error {
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("preparing: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for i := range n {
		if _, err := stmt.ExecContext(ctx, args(i)...); err != nil {
			return err
		}
	}
	return nil
}

// trim runs a cap query and returns how many rows it deleted.
func trim(ctx context.Context, tx *sql.Tx, query string, limit int64) (int64, error) {
	res, err := tx.ExecContext(ctx, query, limit)
	if err != nil {
		return 0, fmt.Errorf("trimming: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("trimming: %w", err)
	}
	return n, nil
}
