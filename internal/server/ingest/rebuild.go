package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/ingest/rollup"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// rebuildSpan is how much history one rebuild transaction recomputes. A week
// keeps each transaction short while needing few round trips.
const rebuildSpan = 7 * 24 * time.Hour

// RebuildUser recomputes all of a user's rollup rows from their stored
// heartbeats. Use it after the duration rule changes. It is idempotent and
// safe to run while ingest continues: each span runs under the user's ingest
// lock, so a concurrent batch either lands before or after it.
func (s *Service) RebuildUser(ctx context.Context, userID uuid.UUID) error {
	var first, last time.Time
	empty := false
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		var err error
		first, err = q.FirstHeartbeatTime(ctx, userID)
		if store.IsNotFound(err) {
			empty = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading first heartbeat: %w", err)
		}
		if last, err = q.LastHeartbeatTime(ctx, userID); err != nil {
			return fmt.Errorf("reading last heartbeat: %w", err)
		}
		return nil
	})
	if err != nil || empty {
		return err
	}
	end := last.Add(rollup.Timeout).UTC().Truncate(time.Minute).Add(time.Minute)
	for from := first.UTC().Truncate(24 * time.Hour); from.Before(end); from = from.Add(rebuildSpan) {
		w := []rollup.Range{{Start: from, End: from.Add(rebuildSpan)}}
		err := s.store.InTx(ctx, func(q *db.Queries) error {
			if err := q.LockUserIngest(ctx, userID); err != nil {
				return fmt.Errorf("locking user ingest: %w", err)
			}
			return recompute(ctx, q, userID, w)
		})
		if err != nil {
			return fmt.Errorf("rebuilding %s: %w", from.Format(time.DateOnly), err)
		}
	}
	if s.pub != nil {
		s.pub.MinutesChanged(userID, first, end)
	}
	return nil
}

// RebuildAll runs RebuildUser for every user with heartbeats.
func (s *Service) RebuildAll(ctx context.Context) (users int, err error) {
	var ids []uuid.UUID
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		var err error
		if ids, err = q.ListUserIDsWithHeartbeats(ctx); err != nil {
			return fmt.Errorf("listing users: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.RebuildUser(ctx, id); err != nil {
			return users, fmt.Errorf("user %s: %w", id, err)
		}
		users++
	}
	return users, nil
}
