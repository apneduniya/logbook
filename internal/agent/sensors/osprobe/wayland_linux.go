//go:build linux

package osprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// waylandNotifyTimeout is the idle timeout asked of the compositor: it sends
// "idled" after this long without input and "resumed" on the next input.
// Idle time is known to this precision; shorter would wake the daemon on
// every pause in typing.
const waylandNotifyTimeout = 5 * time.Second

// waylandReconnect is the least time between connection attempts.
const waylandReconnect = time.Minute

// Object ids this client allocates (wl_display is always 1).
const (
	wlDisplayID uint32 = 1 + iota
	wlRegistryID
	wlSyncID
	wlSeatID
	wlNotifierID
	wlNotificationID
	wlSync2ID
)

// Opcodes used (requests and events numbered in protocol XML order).
const (
	wlDisplaySync        = 0 // request
	wlDisplayGetRegistry = 1 // request
	wlDisplayError       = 0 // event
	wlRegistryBind       = 0 // request
	wlRegistryGlobal     = 0 // event
	wlCallbackDone       = 0 // event

	extNotifierGetIdle      = 1 // get_idle_notification (v1)
	extNotifierGetInputIdle = 2 // get_input_idle_notification (v2)
	extNotificationIdled    = 0 // event
	extNotificationResumed  = 1 // event
)

// waylandIdle reads idle time through the ext-idle-notify-v1 protocol
// (https://wayland.app/protocols/ext-idle-notify-v1). It holds one
// connection to the compositor with one notification object; a goroutine
// reads its idled/resumed events, so each sample is a lock and a clock read,
// with no round trip.
//
// With protocol version 2 it uses get_input_idle_notification, which ignores
// idle inhibitors (a playing video), so it measures real input. With version
// 1 inhibitors count as activity.
type waylandIdle struct {
	socketPath func() (string, error)
	now        func() time.Time

	conn    net.Conn
	retryAt time.Time

	mu      sync.Mutex
	idledAt time.Time // when "idled" arrived; zero while active
	dead    error     // set when the reader stopped
}

func newWaylandIdle(socketPath func() (string, error), now func() time.Time) *waylandIdle {
	return &waylandIdle{socketPath: socketPath, now: now}
}

// waylandSocketPath finds the compositor socket the way libwayland does:
// $WAYLAND_DISPLAY (default "wayland-0") under $XDG_RUNTIME_DIR, or the
// absolute path in $WAYLAND_DISPLAY. Without XDG_RUNTIME_DIR it uses
// /run/user/<uid>, which is where logind puts it.
func waylandSocketPath(getenv func(string) string, uid int) (string, error) {
	name := getenv("WAYLAND_DISPLAY")
	if name == "" {
		name = "wayland-0"
	}
	if filepath.IsAbs(name) {
		return name, nil
	}
	dir := getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		if uid < 0 {
			return "", fmt.Errorf("wayland: no runtime dir: %w", ErrUnavailable)
		}
		dir = "/run/user/" + strconv.Itoa(uid)
	}
	return filepath.Join(dir, name), nil
}

// idle returns the time since the last input, to waylandNotifyTimeout
// precision: 0 while input happened within the timeout.
//
// The protocol starts each notification as "not idle" and counts from its
// creation, so right after connecting idle time reads 0 until the first
// "idled" (at most waylandNotifyTimeout later).
func (w *waylandIdle) idle(ctx context.Context) (time.Duration, error) {
	if w.conn != nil {
		w.mu.Lock()
		idledAt, dead := w.idledAt, w.dead
		w.mu.Unlock()
		if dead == nil {
			if idledAt.IsZero() {
				return 0, nil
			}
			return max(w.now().Sub(idledAt), 0) + waylandNotifyTimeout, nil
		}
		w.close()
		return 0, dead
	}
	if w.now().Before(w.retryAt) {
		return 0, fmt.Errorf("wayland: waiting to reconnect: %w", ErrUnavailable)
	}
	w.retryAt = w.now().Add(waylandReconnect)
	if err := w.connect(ctx); err != nil {
		return 0, err
	}
	return 0, nil
}

// connect dials the compositor, binds a seat and the idle notifier, creates
// the notification and starts the event reader. The handshake runs under a
// deadline of callTimeout.
func (w *waylandIdle) connect(ctx context.Context) error {
	path, err := w.socketPath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("wayland: no compositor socket: %w", ErrUnavailable)
		}
		return fmt.Errorf("wayland: connecting: %w", err)
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close() // setup failed; the close error adds nothing
		return fmt.Errorf("wayland: %w", err)
	}
	if err := handshake(conn); err != nil {
		_ = conn.Close() // setup failed; the close error adds nothing
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close() // setup failed; the close error adds nothing
		return fmt.Errorf("wayland: %w", err)
	}
	w.conn = conn
	w.mu.Lock()
	w.idledAt, w.dead = time.Time{}, nil
	w.mu.Unlock()
	go w.read(conn)
	return nil
}

// handshake performs the registry round trip and creates the notification.
func handshake(conn net.Conn) error {
	buf := make([]byte, wlMaxMessage)
	req := append(new(wlBuild).uint(wlRegistryID).message(wlDisplayID, wlDisplayGetRegistry),
		new(wlBuild).uint(wlSyncID).message(wlDisplayID, wlDisplaySync)...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("wayland: %w", err)
	}
	var seatName, notifierName, notifierVersion uint32
	for done := false; !done; {
		m, err := wlRead(conn, buf)
		if err != nil {
			return fmt.Errorf("wayland: reading globals: %w", err)
		}
		switch {
		case m.object == wlDisplayID && m.opcode == wlDisplayError:
			return displayError(m)
		case m.object == wlRegistryID && m.opcode == wlRegistryGlobal:
			a := wlArgs{b: m.args}
			name, iface, version := a.uint(), a.str(), a.uint()
			if a.err != nil {
				return fmt.Errorf("wayland: registry global: %w", a.err)
			}
			switch {
			case iface == "wl_seat" && seatName == 0:
				seatName = name
			case iface == "ext_idle_notifier_v1":
				notifierName, notifierVersion = name, version
			}
		case m.object == wlSyncID && m.opcode == wlCallbackDone:
			done = true
		}
	}
	if notifierName == 0 || seatName == 0 {
		return fmt.Errorf("wayland: compositor lacks ext-idle-notify-v1 or a seat: %w", ErrUnavailable)
	}
	version, create := uint32(1), uint16(extNotifierGetIdle)
	if notifierVersion >= 2 {
		version, create = 2, extNotifierGetInputIdle
	}
	timeoutMs := uint32(waylandNotifyTimeout / time.Millisecond)
	var out []byte
	out = append(out, new(wlBuild).uint(seatName).str("wl_seat").uint(1).uint(wlSeatID).message(wlRegistryID, wlRegistryBind)...)
	out = append(out, new(wlBuild).uint(notifierName).str("ext_idle_notifier_v1").uint(version).uint(wlNotifierID).
		message(wlRegistryID, wlRegistryBind)...)
	out = append(out, new(wlBuild).uint(wlNotificationID).uint(timeoutMs).uint(wlSeatID).message(wlNotifierID, create)...)
	out = append(out, new(wlBuild).uint(wlSync2ID).message(wlDisplayID, wlDisplaySync)...)
	if _, err := conn.Write(out); err != nil {
		return fmt.Errorf("wayland: %w", err)
	}
	// Wait for the second sync so a protocol error surfaces here, not later.
	for {
		m, err := wlRead(conn, buf)
		if err != nil {
			return fmt.Errorf("wayland: creating idle notification: %w", err)
		}
		switch {
		case m.object == wlDisplayID && m.opcode == wlDisplayError:
			return displayError(m)
		case m.object == wlSync2ID && m.opcode == wlCallbackDone:
			return nil
		}
	}
}

// read handles events until the connection fails or is closed. It only
// keeps a fixed buffer, so its memory is bounded.
func (w *waylandIdle) read(conn net.Conn) {
	buf := make([]byte, wlMaxMessage)
	for {
		m, err := wlRead(conn, buf)
		if err == nil && m.object == wlDisplayID && m.opcode == wlDisplayError {
			err = displayError(m)
		}
		if err != nil {
			w.mu.Lock()
			w.dead = fmt.Errorf("wayland: connection lost: %w", err)
			w.mu.Unlock()
			return
		}
		if m.object != wlNotificationID {
			continue // seat capabilities, delete_id and the like
		}
		w.mu.Lock()
		switch m.opcode {
		case extNotificationIdled:
			w.idledAt = w.now()
		case extNotificationResumed:
			w.idledAt = time.Time{}
		}
		w.mu.Unlock()
	}
}

// displayError decodes wl_display.error. The message is the compositor's
// own description of our protocol mistake.
func displayError(m wlMessage) error {
	a := wlArgs{b: m.args}
	object, code, msg := a.uint(), a.uint(), a.str()
	if a.err != nil {
		return fmt.Errorf("wayland: protocol error: %w", a.err)
	}
	return fmt.Errorf("wayland: protocol error on object %d (code %d): %s", object, code, msg)
}

func (w *waylandIdle) close() {
	if w.conn != nil {
		_ = w.conn.Close() // stops the reader; nothing to do on error
		w.conn = nil
	}
}
