package oauth

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Retention of dead rows. Used codes and revoked tokens are kept a while so
// a replay in that time is still recognised and revokes its family.
const (
	codeRetention  = 24 * time.Hour
	tokenRetention = 30 * 24 * time.Hour
)

// Cleanup deletes expired consent requests, codes past codeRetention, and
// tokens dead for longer than tokenRetention. It returns how many rows went.
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	now := s.now()
	var total int64
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		n, err := q.OAuthDeleteExpiredRequests(ctx, now)
		if err != nil {
			return fmt.Errorf("deleting consent requests: %w", err)
		}
		total += n
		if n, err = q.OAuthDeleteExpiredCodes(ctx, now.Add(-codeRetention)); err != nil {
			return fmt.Errorf("deleting codes: %w", err)
		}
		total += n
		if n, err = q.OAuthDeleteDeadTokens(ctx, now.Add(-tokenRetention)); err != nil {
			return fmt.Errorf("deleting tokens: %w", err)
		}
		total += n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// RunCleanup calls Cleanup every interval until ctx is done.
func (s *Service) RunCleanup(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Cleanup(ctx); err != nil {
				slog.ErrorContext(ctx, "oauth cleanup failed", "err", err)
			} else if n > 0 {
				slog.InfoContext(ctx, "oauth cleanup", "deleted", n)
			}
		}
	}
}
