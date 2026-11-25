package oauth

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Retention of dead rows once their family is dead too. A used code or a
// rotated refresh token is the record that replaying it revokes its family,
// so it is never deleted while any token of that family is still usable
// (access tokens live a year, refresh tokens two); these retentions only
// start counting for a family nothing can use any more.
//
// There is deliberately no absolute bound: a family that keeps being
// refreshed keeps one row per rotation for as long as it lives, because
// deleting an old rotated token while its descendants work would let that
// token be replayed without revoking them.
const (
	codeRetention  = 24 * time.Hour
	tokenRetention = 30 * 24 * time.Hour
)

// Cleanup deletes expired consent requests, codes past codeRetention, and
// tokens dead for longer than tokenRetention, keeping every code and token
// of a family with a usable token. It returns how many rows went.
//
// Each family is decided in its own transaction under its family lock
// (OAuthFamilyLock), the lock a refresh or code exchange holds while it
// issues into the family. A family whose last token is being refreshed is
// therefore judged after the new token exists, never just before. Nothing
// else is locked before the family lock, and the family's codes go before
// its tokens, the order RevokeGrant and DeleteApp touch them in.
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	now := s.now()
	total, err := s.st.OAuthDeleteExpiredRequests(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("deleting consent requests: %w", err)
	}
	codeBefore, tokenBefore := now.Add(-codeRetention), now.Add(-tokenRetention)
	families, err := s.st.OAuthCleanupFamilies(ctx, db.OAuthCleanupFamiliesParams{CodeBefore: codeBefore, TokenBefore: tokenBefore})
	if err != nil {
		return total, fmt.Errorf("listing families to clean up: %w", err)
	}
	for _, family := range families {
		n, err := s.cleanupFamily(ctx, family, now, codeBefore, tokenBefore)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// cleanupFamily deletes one family's dead codes and tokens, if nothing in
// the family is usable at now.
func (s *Service) cleanupFamily(ctx context.Context, family uuid.UUID, now, codeBefore, tokenBefore time.Time) (int64, error) {
	var total int64
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		if err := q.OAuthFamilyLock(ctx, family); err != nil {
			return fmt.Errorf("locking token family: %w", err)
		}
		n, err := q.OAuthFamilyDeleteDeadCodes(ctx, db.OAuthFamilyDeleteDeadCodesParams{FamilyID: family, Before: codeBefore, Now: now})
		if err != nil {
			return fmt.Errorf("deleting codes: %w", err)
		}
		total += n
		if n, err = q.OAuthFamilyDeleteDeadTokens(ctx, db.OAuthFamilyDeleteDeadTokensParams{FamilyID: family, Before: tokenBefore, Now: now}); err != nil {
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
