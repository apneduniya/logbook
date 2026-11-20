package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Restart backoff for supervised tasks.
const (
	restartDefaultFloor = time.Second
	restartMax          = 5 * time.Minute
	healthyAfter        = time.Minute // a run this long resets the backoff
	maxStackBytes       = 4 << 10
)

// supervise runs fn until ctx is done, restarting it with backoff whenever
// it returns or panics. It never returns an error: one broken task must not
// stop the daemon.
func supervise(ctx context.Context, log *slog.Logger, name string, minWait time.Duration, fn func(ctx context.Context) error) {
	wait := minWait
	for {
		started := time.Now()
		err := runSafely(ctx, fn)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= healthyAfter {
			wait = minWait
		}
		log.WarnContext(ctx, "task stopped; restarting", "task", name, "err", err, "in", wait)
		if sleep(ctx, wait) != nil {
			return
		}
		wait = min(wait*2, restartMax)
	}
}

// runSafely calls fn, turning a panic into an error.
func runSafely(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			stack := debug.Stack()
			if len(stack) > maxStackBytes {
				stack = stack[:maxStackBytes]
			}
			err = fmt.Errorf("panic: %v\n%s", p, stack)
		}
	}()
	if err := fn(ctx); err != nil {
		return err
	}
	return fmt.Errorf("returned before shutdown")
}

// every runs fn now and then every interval until ctx is done.
func every(ctx context.Context, interval time.Duration, fn func(ctx context.Context)) error {
	fn(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			fn(ctx)
		}
	}
}
