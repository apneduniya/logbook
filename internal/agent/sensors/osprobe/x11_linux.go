//go:build linux

package osprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/res"
	"github.com/jezek/xgb/screensaver"
	"github.com/jezek/xgb/xproto"
)

// x11Reconnect is how long to wait before reconnecting after a failure.
const x11Reconnect = time.Minute

// x11Client is a lazily opened, reused connection to the X server. It reads
// only the root window's _NET_ACTIVE_WINDOW, the PID of the client owning
// that window (X-Resource), and, without X-Resource, that window's
// _NET_WM_PID and WM_CLIENT_MACHINE; plus the MIT-SCREEN-SAVER idle counter.
// Never window names or contents.
type x11Client struct {
	display string

	conn       *xgb.Conn
	dialing    boundedDial[*xgb.Conn]
	root       xproto.Window
	activeAtom xproto.Atom
	pidAtom    xproto.Atom
	saver      bool // MIT-SCREEN-SAVER extension present
	xres       bool // X-Resource 1.2+ present (QueryClientIds)
	host       string
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

// x11Window is what is known about the active window's owner.
type x11Window struct {
	xresOK  bool   // the X server answered QueryClientIds
	xresPID uint32 // host PID of the owning client, 0 if the server doesn't know (remote client)
	wmPID   uint32 // _NET_WM_PID as the client published it
	machine string // WM_CLIENT_MACHINE as the client published it
}

// focusPID picks the PID to trust for the focused window.
//
// The X server learns a local client's PID from the socket (SO_PEERCRED),
// in its own PID namespace, so X-Resource gives the right host PID even for
// Flatpak apps and other sandboxes. _NET_WM_PID is whatever the client wrote:
// inside a Flatpak PID namespace it is the in-sandbox PID (often 2, which is
// kthreadd on the host), and for a remote (ssh -X) client it is a PID on
// another machine. So _NET_WM_PID is used only when the server lacks
// X-Resource, and only when WM_CLIENT_MACHINE names this host; a Flatpak app
// can still be misnamed in that fallback (it shares the hostname).
func focusPID(w x11Window, host string) int {
	if w.xresOK {
		return int(w.xresPID)
	}
	if w.machine != "" && host != "" && w.machine != host {
		return 0
	}
	return int(w.wmPID)
}

// focusedPID returns the host PID of the client owning the window manager's
// active window, or 0 when unknown.
func (x *x11Client) focusedPID(ctx context.Context) (int, error) {
	if err := x.ensure(ctx); err != nil {
		return 0, err
	}
	if x.activeAtom == 0 {
		return 0, nil // the window manager doesn't publish the active window
	}
	var info x11Window
	found := false
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
		found = true
		info = x.windowOwner(c, w)
		return nil
	})
	if err != nil || !found {
		return 0, err
	}
	return focusPID(info, x.host), nil
}

// windowOwner gathers what focusPID needs. A window can close between
// requests; failed lookups leave fields empty, which reads as unknown.
func (x *x11Client) windowOwner(c *xgb.Conn, w xproto.Window) x11Window {
	var info x11Window
	if x.xres {
		spec := []res.ClientIdSpec{{Client: uint32(w), Mask: res.ClientIdMaskLocalClientPID}}
		if r, err := res.QueryClientIds(c, 1, spec).Reply(); err == nil && r != nil {
			info.xresOK = true
			for _, id := range r.Ids {
				if id.Spec.Mask&res.ClientIdMaskLocalClientPID != 0 && len(id.Value) > 0 {
					info.xresPID = id.Value[0]
				}
			}
		}
		if info.xresOK {
			return info
		}
	}
	if x.pidAtom != 0 {
		if p, err := xproto.GetProperty(c, false, w, x.pidAtom, xproto.AtomCardinal, 0, 1).Reply(); err == nil &&
			p.Format == 32 && len(p.Value) >= 4 {
			info.wmPID = xgb.Get32(p.Value)
		}
	}
	if m, err := xproto.GetProperty(c, false, w, xproto.AtomWmClientMachine, xproto.AtomString, 0, 64).Reply(); err == nil &&
		m.Format == 8 {
		info.machine = string(m.Value)
	}
	return info
}

// ensure connects (at most once per x11Reconnect after a failure) and looks
// up the atoms and extensions once per connection. The connect itself
// (socket, authorisation, setup handshake) is bounded by callTimeout.
func (x *x11Client) ensure(ctx context.Context) error {
	if x.conn != nil {
		return nil
	}
	if x.dialing.inflight == nil {
		if time.Now().Before(x.retryAt) {
			return fmt.Errorf("x11: waiting to reconnect: %w", ErrUnavailable)
		}
		x.retryAt = time.Now().Add(x11Reconnect)
	}
	conn, err := x.dialing.dial(ctx, callTimeout, func() (*xgb.Conn, error) { return xgb.NewConnDisplay(x.display) })
	if err != nil {
		return fmt.Errorf("connecting to X display: %w", err)
	}
	x.conn = conn
	x.host, _ = os.Hostname() // empty on error: focusPID then skips the host check
	err = x.do(ctx, func(c *xgb.Conn) error {
		x.root = xproto.Setup(c).DefaultScreen(c).Root
		x.saver = screensaver.Init(c) == nil
		x.xres = false
		if res.Init(c) == nil {
			// QueryClientIds needs X-Resource 1.2 (Xorg 1.14, 2013).
			if v, err := res.QueryVersion(c, 1, 2).Reply(); err == nil &&
				(v.ServerMajor > 1 || (v.ServerMajor == 1 && v.ServerMinor >= 2)) {
				x.xres = true
			}
		}
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
	x.dialing.abandon(func(c *xgb.Conn) { c.Close() })
}

func internAtom(c *xgb.Conn, name string) (xproto.Atom, error) {
	r, err := xproto.InternAtom(c, true, uint16(len(name)), name).Reply() //nolint:gosec // constant atom names are short
	if err != nil {
		return 0, fmt.Errorf("x11 intern %s: %w", name, err)
	}
	return r.Atom, nil // 0 when no client defined it (no EWMH window manager)
}
