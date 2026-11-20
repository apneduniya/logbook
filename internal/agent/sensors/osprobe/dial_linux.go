//go:build linux

package osprobe

import (
	"context"
	"fmt"
	"time"
)

// dialResult is the outcome of one background connect.
type dialResult[T any] struct {
	conn T
	err  error
}

// boundedDial runs a blocking connect (D-Bus auth + Hello, the X11 setup
// handshake) in the background, so a wedged server can delay the caller by
// at most the timeout. At most one connect is in flight: when it outlives
// the timeout, later calls wait on the same attempt instead of starting
// another, and a late success is handed to the next caller. Not safe for
// concurrent use.
type boundedDial[T any] struct {
	inflight chan dialResult[T] // nil when no connect is running
}

// dial returns the result of connect, or an error wrapping
// context.DeadlineExceeded when it doesn't finish within timeout.
func (b *boundedDial[T]) dial(ctx context.Context, timeout time.Duration, connect func() (T, error)) (T, error) {
	if b.inflight == nil {
		ch := make(chan dialResult[T], 1)
		b.inflight = ch
		go func() {
			c, err := connect()
			ch <- dialResult[T]{c, err}
		}()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case r := <-b.inflight:
		b.inflight = nil
		return r.conn, r.err
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("connect still pending: %w", ctx.Err())
	}
}

// abandon releases a connect still in flight: when it finishes, a
// successful connection is passed to release (to close it).
func (b *boundedDial[T]) abandon(release func(T)) {
	ch := b.inflight
	if ch == nil {
		return
	}
	b.inflight = nil
	go func() {
		if r := <-ch; r.err == nil {
			release(r.conn)
		}
	}()
}
