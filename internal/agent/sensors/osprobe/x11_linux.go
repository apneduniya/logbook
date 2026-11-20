//go:build linux

package osprobe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/screensaver"
	"github.com/jezek/xgb/xproto"
)

// x11Reconnect is how long to wait before reconnecting after a failure.
const x11Reconnect = time.Minute

// x11Client is a lazily opened, reused connection to the X server. It reads
// only the root window's _NET_ACTIVE_WINDOW, that window's _NET_WM_PID and
// the MIT-SCREEN-SAVER idle counter; never window names or contents.
type x11Client struct {
	display string

	conn       *xgb.Conn
	root       xproto.Window
	activeAtom xproto.Atom
	pidAtom    xproto.Atom
	saver      bool // MIT-SCREEN-SAVER extension present
	retryAt    time.Time
}

func newX11Client(display string) *x11Client { return &x11Client{display: display} }

// idle returns MsSinceUserInput from the MIT-SCREEN-SAVER extension.
func (x *x11Client) idle(ctx context.Context) (time.Duration, error) {
	if err := x.ensure(ctx); err != nil {
		return 0, err
	}
	if !x.saver {
		return 0, fmt.Errorf("x11: MIT-SCREEN-SAVER extension missing: %w", ErrUnavailable)
	}
	var reply *screensaver.QueryInfoReply
	err := x.do(ctx, func(c *xgb.Conn) (err error) {
		reply, err = screensaver.QueryInfo(c, xproto.Drawable(x.root)).Reply()
		return err
	})
	if err != nil {
		return 0, err
	}
	return time.Duration(reply.MsSinceUserInput) * time.Millisecond, nil
}

// activePID returns the PID of the window manager's active window, or 0.
func (x *x11Client) activePID(ctx context.Context) (int, error) {
	if err := x.ensure(ctx); err != nil {
		return 0, err
	}
	if x.activeAtom == 0 || x.pidAtom == 0 {
		return 0, nil // the window manager doesn't publish the active window
	}
	var pid int
	err := x.do(ctx, func(c *xgb.Conn) error {
		win, err := xproto.GetProperty(c, false, x.root, x.activeAtom, xproto.AtomWindow, 0, 1).Reply()
		if err != nil {
			return fmt.Errorf("x11 _NET_ACTIVE_WINDOW: %w", err)
		}
		if win.Format != 32 || len(win.Value) < 4 {
			return nil // no active window (desktop focused, or no EWMH window manager)
		}
		w := xproto.Window(xgb.Get32(win.Value))
		if w == 0 {
			return nil
		}
		prop, err := xproto.GetProperty(c, false, w, x.pidAtom, xproto.AtomCardinal, 0, 1).Reply()
		if err != nil {
			// The window can close between the two requests; treat as unknown.
			return nil //nolint:nilerr // a vanished window is not an error
		}
		if prop.Format == 32 && len(prop.Value) >= 4 {
			pid = int(xgb.Get32(prop.Value))
		}
		return nil
	})
	return pid, err
}

// ensure connects (at most once per x11Reconnect after a failure) and looks
// up the atoms and extension once per connection.
func (x *x11Client) ensure(ctx context.Context) error {
	if x.conn != nil {
		return nil
	}
	if time.Now().Before(x.retryAt) {
		return fmt.Errorf("x11: waiting to reconnect: %w", ErrUnavailable)
	}
	x.retryAt = time.Now().Add(x11Reconnect)
	conn, err := xgb.NewConnDisplay(x.display)
	if err != nil {
		return fmt.Errorf("connecting to X display: %w", err)
	}
	x.conn = conn
	err = x.do(ctx, func(c *xgb.Conn) error {
		x.root = xproto.Setup(c).DefaultScreen(c).Root
		x.saver = screensaver.Init(c) == nil
		var err error
		if x.activeAtom, err = internAtom(c, "_NET_ACTIVE_WINDOW"); err != nil {
			return err
		}
		x.pidAtom, err = internAtom(c, "_NET_WM_PID")
		return err
	})
	if err != nil {
		x.close()
		return err
	}
	return nil
}

// do runs fn against the connection with a timeout. xgb requests can't be
// cancelled, so on timeout the connection is closed, which fails the pending
// request and lets fn's goroutine return.
func (x *x11Client) do(ctx context.Context, fn func(*xgb.Conn) error) error {
	conn := x.conn
	if conn == nil {
		return errors.New("x11: not connected")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(conn) }()
	select {
	case err := <-done:
		if err != nil {
			x.close()
		}
		return err
	case <-ctx.Done():
		x.close()
		return fmt.Errorf("x11 request: %w", ctx.Err())
	}
}

func (x *x11Client) close() {
	if x.conn != nil {
		x.conn.Close()
		x.conn = nil
	}
}

func internAtom(c *xgb.Conn, name string) (xproto.Atom, error) {
	r, err := xproto.InternAtom(c, true, uint16(len(name)), name).Reply() //nolint:gosec // constant atom names are short
	if err != nil {
		return 0, fmt.Errorf("x11 intern %s: %w", name, err)
	}
	return r.Atom, nil // 0 when no client defined it (no EWMH window manager)
}
