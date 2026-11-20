//go:build linux

package osprobe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

var _ prober = (*Probe)(nil)

// callTimeout bounds every single OS call (D-Bus round trip, X11 request).
const callTimeout = time.Second

// Probe reads idle time, focus and lock state on Linux. Connections to the
// session bus, the system bus, the Wayland compositor and the X server are
// opened lazily, reused across samples and reopened after failures. Not safe
// for concurrent use.
type Probe struct {
	log     *slog.Logger
	now     func() time.Time
	session linuxSession
	idle    *idleChain
	sess    *bus
	sys     *bus
	x       *x11Client
	wl      *waylandIdle

	logindSession    dbus.ObjectPath // the user's display session, cached
	logind           logindState
	nextSessionCheck time.Time
	focusWarned      bool
}

// New returns a Linux probe. It does not connect to anything until first use.
func New(log *slog.Logger) *Probe {
	p := &Probe{
		log:  log,
		now:  time.Now,
		sess: &bus{connect: sessionBus},
		sys:  &bus{connect: systemBus},
	}
	p.setSession(detectSession(os.Getenv))
	p.wl = newWaylandIdle(func() (string, error) { return waylandSocketPath(os.Getenv, os.Getuid()) }, time.Now)
	byName := map[string]func(context.Context) (time.Duration, error){
		srcMutter:      p.mutterIdle,
		srcScreenSaver: p.screenSaverIdle,
		srcWayland:     p.waylandIdle,
		srcX11:         p.x11Idle,
		srcLogind:      p.logindIdle,
	}
	var sources []idleSource
	for _, name := range linuxIdleOrder {
		sources = append(sources, idleSource{name: name, read: byName[name]})
	}
	p.idle = newIdleChain(sources, time.Now, log)
	return p
}

// setSession records the session type and opens the X11 client for it.
func (p *Probe) setSession(s linuxSession) {
	p.session = s
	if s.x11 && p.x == nil {
		p.x = newX11Client(s.display)
	}
}

// refreshSession asks logind for the session type while it is unknown (a
// systemd --user service started before the desktop exported DISPLAY or
// WAYLAND_DISPLAY), at most once per rescanEvery.
func (p *Probe) refreshSession(ctx context.Context) {
	if p.session.known() || p.now().Before(p.nextSessionCheck) {
		return
	}
	p.nextSessionCheck = p.now().Add(rescanEvery)
	typ, err := p.logindString(ctx, "Type")
	if err != nil {
		return
	}
	display, err := p.logindString(ctx, "Display")
	if err != nil {
		return
	}
	if s := sessionFromLogind(typ, display); s.known() {
		p.log.Info("session type learned from logind", "type", typ)
		p.setSession(s)
	}
}

// IdleTime returns the idle time from the first working source.
func (p *Probe) IdleTime(ctx context.Context) (time.Duration, error) {
	p.refreshSession(ctx)
	return p.idle.Idle(ctx)
}

// FocusedApp returns the focused window's process name on X11. On Wayland
// there is no portable, permission-free way to learn the focused app, so it
// returns "" (unknown).
func (p *Probe) FocusedApp(ctx context.Context) (string, error) {
	p.refreshSession(ctx)
	if p.x == nil {
		if !p.focusWarned {
			p.focusWarned = true
			p.log.Info("focused app is unknown on this session (Wayland or no DISPLAY)")
		}
		return "", nil
	}
	pid, err := p.x.focusedPID(ctx)
	if err != nil || pid <= 0 {
		return "", err
	}
	return processName(pid)
}

// ScreenLocked returns logind's LockedHint for the user's display session.
// Desktops that don't set it read as unlocked; idle time still triggers the
// sensor's slow cadence there.
func (p *Probe) ScreenLocked(ctx context.Context) (bool, error) {
	v, err := p.logindProp(ctx, "LockedHint")
	if err != nil {
		return false, err
	}
	locked, ok := v.Value().(bool)
	if !ok {
		return false, fmt.Errorf("logind LockedHint has type %s", v.Signature())
	}
	return locked, nil
}

// Sources describes the backends in use.
func (p *Probe) Sources() map[string]string {
	focus := "unavailable (Wayland or no DISPLAY)"
	if p.x != nil {
		focus = "x11 _NET_ACTIVE_WINDOW + X-Resource client PID"
	}
	return map[string]string{"idle": p.idle.Current(), "focus": focus, "lock": "logind LockedHint"}
}

// Close closes every open connection.
func (p *Probe) Close() error {
	p.sess.close()
	p.sys.close()
	p.wl.close()
	if p.x != nil {
		p.x.close()
	}
	return nil
}

func (p *Probe) mutterIdle(ctx context.Context) (time.Duration, error) {
	var ms uint64
	err := p.sess.call(ctx, "org.gnome.Mutter.IdleMonitor", "/org/gnome/Mutter/IdleMonitor/Core",
		"org.gnome.Mutter.IdleMonitor.GetIdletime", &ms)
	return time.Duration(ms) * time.Millisecond, err //nolint:gosec // idle ms fits easily in int64 nanoseconds
}

func (p *Probe) screenSaverIdle(ctx context.Context) (time.Duration, error) {
	var ms uint32
	err := p.sess.call(ctx, "org.freedesktop.ScreenSaver", "/org/freedesktop/ScreenSaver",
		"org.freedesktop.ScreenSaver.GetSessionIdleTime", &ms)
	return time.Duration(ms) * time.Millisecond, err
}

func (p *Probe) waylandIdle(ctx context.Context) (time.Duration, error) {
	if !p.session.allows(srcWayland) {
		return 0, fmt.Errorf("wayland: X11 session: %w", ErrUnavailable)
	}
	return p.wl.idle(ctx)
}

func (p *Probe) x11Idle(ctx context.Context) (time.Duration, error) {
	if p.x == nil || !p.session.allows(srcX11) {
		return 0, ErrUnavailable
	}
	return p.x.idle(ctx)
}

// logindIdle reads IdleHint/IdleSinceHint; logindState decides whether they
// can be trusted on this desktop.
func (p *Probe) logindIdle(ctx context.Context) (time.Duration, error) {
	v, err := p.logindProp(ctx, "IdleHint")
	if err != nil {
		return 0, err
	}
	hint, ok := v.Value().(bool)
	if !ok {
		return 0, fmt.Errorf("logind IdleHint has type %s", v.Signature())
	}
	var since uint64
	if hint {
		sv, err := p.logindProp(ctx, "IdleSinceHint")
		if err != nil {
			return 0, err
		}
		since, _ = sv.Value().(uint64) // a wrong type reads as 0: idle, duration unknown
	}
	return p.logind.idle(string(p.logindSession), hint, since, p.now())
}

// logindString reads a string property of the user's display session.
func (p *Probe) logindString(ctx context.Context, name string) (string, error) {
	v, err := p.logindProp(ctx, name)
	if err != nil {
		return "", err
	}
	str, ok := v.Value().(string)
	if !ok {
		return "", fmt.Errorf("logind %s has type %s", name, v.Signature())
	}
	return str, nil
}

// logindProp reads a property of the user's display session. The session is
// found through /org/freedesktop/login1/user/self, which works for a
// systemd --user service that is not itself inside the graphical session.
func (p *Probe) logindProp(ctx context.Context, name string) (dbus.Variant, error) {
	if p.logindSession == "" {
		var display struct {
			ID   string
			Path dbus.ObjectPath
		}
		var v dbus.Variant
		if err := p.sys.call(ctx, "org.freedesktop.login1", "/org/freedesktop/login1/user/self",
			"org.freedesktop.DBus.Properties.Get", &v, "org.freedesktop.login1.User", "Display"); err != nil {
			return dbus.Variant{}, err
		}
		if err := dbus.Store([]any{v.Value()}, &display); err != nil {
			return dbus.Variant{}, fmt.Errorf("parsing logind Display: %w", err)
		}
		if display.Path == "" || display.Path == "/" {
			return dbus.Variant{}, fmt.Errorf("logind: no graphical session: %w", ErrUnavailable)
		}
		p.logindSession = display.Path
	}
	var v dbus.Variant
	err := p.sys.call(ctx, "org.freedesktop.login1", p.logindSession,
		"org.freedesktop.DBus.Properties.Get", &v, "org.freedesktop.login1.Session", name)
	if err != nil {
		p.logindSession = "" // the session may have ended; look it up again next time
		return dbus.Variant{}, err
	}
	return v, nil
}

// sessionBus connects to the session bus without ever launching one: the
// library's ConnectSessionBus runs dbus-launch when no bus address is known,
// which a background sensor must not do.
func sessionBus() (*dbus.Conn, error) {
	return handshakeBus(dbus.SessionBusPrivateNoAutoStartup())
}

func systemBus() (*dbus.Conn, error) { return handshakeBus(dbus.SystemBusPrivate()) }

// handshakeBus authenticates a freshly dialled bus connection and registers
// it (Hello), closing it on failure.
func handshakeBus(conn *dbus.Conn, err error) (*dbus.Conn, error) {
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close() // setup failed; the close error adds nothing
		return nil, fmt.Errorf("d-bus auth: %w", err)
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close() // setup failed; the close error adds nothing
		return nil, fmt.Errorf("d-bus hello: %w", err)
	}
	return conn, nil
}

// bus is a lazily opened, reused D-Bus connection.
type bus struct {
	connect func() (*dbus.Conn, error)
	conn    *dbus.Conn
	dialing boundedDial[*dbus.Conn]
}

// call invokes method on dest/path with a per-call timeout and stores the
// single reply value in out. Connecting is bounded by the same timeout.
func (b *bus) call(ctx context.Context, dest string, path dbus.ObjectPath, method string, out any, args ...any) error {
	if b.conn == nil || !b.conn.Connected() {
		b.closeConn()
		conn, err := b.dialing.dial(ctx, callTimeout, b.connect)
		if err != nil {
			return fmt.Errorf("connecting to d-bus: %w", err)
		}
		b.conn = conn
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	// NoAutoStart: probing must never launch a D-Bus service as a side effect.
	call := b.conn.Object(dest, path).CallWithContext(ctx, method, dbus.FlagNoAutoStart, args...)
	if call.Err != nil {
		return fmt.Errorf("d-bus %s: %w", method, call.Err)
	}
	if err := call.Store(out); err != nil {
		return fmt.Errorf("d-bus %s reply: %w", method, err)
	}
	return nil
}

func (b *bus) closeConn() {
	if b.conn != nil {
		_ = b.conn.Close() // closing a broken connection; nothing to do on error
		b.conn = nil
	}
}

func (b *bus) close() {
	b.closeConn()
	b.dialing.abandon(func(c *dbus.Conn) { _ = c.Close() }) // a late connection nobody will use
}

// processName reads /proc/<pid>/comm, the kernel's process name (at most 15
// bytes). It never reads the command line, which could contain file names.
func processName(pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil // the process exited between the X11 lookup and now
	}
	if err != nil {
		return "", fmt.Errorf("reading process name: %w", err)
	}
	return truncateName(strings.TrimSpace(string(b))), nil
}
