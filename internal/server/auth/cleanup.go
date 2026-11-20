package auth

import (
	"context"
	"log/slog"
	"time"
)

// DefaultSessionCleanupInterval is used when RunSessionCleanup gets a
// non-positive interval.
const DefaultSessionCleanupInterval = time.Hour

// RunSessionCleanup calls CleanupExpiredSessions every interval until ctx is
// cancelled. Errors are logged and retried on the next tick. An interval of
// zero or less (an unset config value) means DefaultSessionCleanupInterval.
func (s *Service) RunSessionCleanup(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(cleanupInterval(interval))
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

func cleanupInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultSessionCleanupInterval
	}
	return d
}
