package oauth

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Retention of dead rows once their family is dead too. A used code or a
// rotated refresh token is the record that replaying it revokes its family,
// so it is never deleted while any token of that family is still usable
// (access tokens live a year, refresh tokens two); these retentions only
// start counting for a family nothing can use any more.
const (
	codeRetention  = 24 * time.Hour
	tokenRetention = 30 * 24 * time.Hour
)

// Cleanup deletes expired consent requests, codes past codeRetention, and
// tokens dead for longer than tokenRetention, keeping every code and token
// of a family with a usable token. It returns how many rows went.
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	now := s.now()
	var total int64
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		n, err := q.OAuthDeleteExpiredRequests(ctx, now)
		if err != nil {
			return fmt.Errorf("deleting consent requests: %w", err)
		}
		total += n
		if n, err = q.OAuthDeleteExpiredCodes(ctx, db.OAuthDeleteExpiredCodesParams{Before: now.Add(-codeRetention), Now: now}); err != nil {
			return fmt.Errorf("deleting codes: %w", err)
		}
		total += n
		if n, err = q.OAuthDeleteDeadTokens(ctx, db.OAuthDeleteDeadTokensParams{Before: now.Add(-tokenRetention), Now: now}); err != nil {
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
