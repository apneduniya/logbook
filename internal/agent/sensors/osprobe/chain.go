package osprobe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// rescanEvery is how often the chain retries sources after none worked, and
// how often it tries to move back to a better source than the current one
// (for example Mutter starting after the daemon at login).
const rescanEvery = 5 * time.Minute

// maxTimeouts is how many samples in a row the current source may time out
// before the chain gives up on it. A single slow reply (a busy compositor)
// costs one sample, not a 5 minute switch to a worse source.
const maxTimeouts = 3

// idleSource is one way of reading idle time.
type idleSource struct {
	name string
	read func(ctx context.Context) (time.Duration, error)
}

// idleChain tries idle sources in priority order and remembers the first
// that works, so steady-state sampling is a single call. It is not safe for
// concurrent use; the activity sensor samples from one goroutine.
type idleChain struct {
	sources []idleSource
	now     func() time.Time
	log     *slog.Logger

	current     int       // index of the working source, -1 if none
	nextScan    time.Time // when to retry after every source failed
	nextUpgrade time.Time // when to try sources better than current
	warned      bool      // logged that nothing works, until something does
	timeouts    int       // consecutive timeouts of the current source
}

func newIdleChain(sources []idleSource, now func() time.Time, log *slog.Logger) *idleChain {
	return &idleChain{sources: sources, now: now, log: log, current: -1}
}

// Idle returns the idle time from the best working source, or ErrUnavailable.
func (c *idleChain) Idle(ctx context.Context) (time.Duration, error) {
	now := c.now()
	if c.current > 0 && !now.Before(c.nextUpgrade) {
		c.nextUpgrade = now.Add(rescanEvery)
		if d, ok := c.scan(ctx, c.current); ok {
			return d, nil
		}
	}
	if c.current >= 0 {
		src := c.sources[c.current]
		d, err := src.read(ctx)
		if err == nil {
			c.timeouts = 0
			return d, nil
		}
		if ctx.Err() != nil {
			// The caller gave up (shutdown, or its own deadline): that
			// says nothing about the source.
			return 0, fmt.Errorf("%s: %w", src.name, ctx.Err())
		}
		if errors.Is(err, context.DeadlineExceeded) && c.timeouts+1 < maxTimeouts {
			c.timeouts++
			return 0, fmt.Errorf("%s timed out, retrying next sample: %w", src.name, ErrUnavailable)
		}
		c.log.Info("idle source stopped working, looking for another", "source", src.name, "err", err)
		c.current = -1
		c.timeouts = 0
	} else if now.Before(c.nextScan) {
		return 0, ErrUnavailable
	}
	if d, ok := c.scan(ctx, len(c.sources)); ok {
		return d, nil
	}
	c.nextScan = now.Add(rescanEvery)
	return 0, ErrUnavailable
}

// scan tries sources[:limit] in order and switches to the first that works.
func (c *idleChain) scan(ctx context.Context, limit int) (time.Duration, bool) {
	var errs []error
	for i, src := range c.sources[:limit] {
		d, err := src.read(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.name, err))
			continue
		}
		if c.current != i {
			c.log.Info("using idle source", "source", src.name)
		}
		c.current = i
		c.timeouts = 0
		c.warned = false
		c.nextUpgrade = c.now().Add(rescanEvery)
		return d, true
	}
	if limit == len(c.sources) && !c.warned {
		c.warned = true
		c.log.Warn("no idle source works; idle time is unknown and the daemon falls back to heartbeats",
			"err", errors.Join(errs...))
	}
	return 0, false
}

// Current returns the name of the source in use, or "none".
func (c *idleChain) Current() string {
	if c.current < 0 {
		return "none"
	}
	return c.sources[c.current].name
}
