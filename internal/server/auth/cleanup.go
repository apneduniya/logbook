package auth

import (
	"context"
	"log/slog"
	"time"
)

// RunSessionCleanup calls CleanupExpiredSessions every interval until ctx is
// cancelled. Errors are logged and retried on the next tick.
func (s *Service) RunSessionCleanup(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := s.CleanupExpiredSessions(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.ErrorContext(ctx, "session cleanup failed", "err", err)
		case n > 0:
			slog.InfoContext(ctx, "expired sessions removed", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
