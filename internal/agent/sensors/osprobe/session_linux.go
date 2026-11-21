//go:build linux

package osprobe

import (
	"fmt"
	"time"
)

// Names of the Linux idle sources, in the order they are tried.
const (
	srcMutter      = "gnome-mutter-idlemonitor"
	srcScreenSaver = "freedesktop-screensaver"
	srcWayland     = "wayland-ext-idle-notify"
	srcX11         = "x11-screensaver-ext"
	srcLogind      = "logind-idlehint"
)

// linuxIdleOrder lists the idle sources, best first:
//
//  1. Mutter's IdleMonitor (GNOME, X11 and Wayland): precise.
//  2. org.freedesktop.ScreenSaver GetSessionIdleTime: precise. KDE Plasma
//     answers it on X11 only; on Wayland kscreenlocker replies NotSupported,
//     so Plasma Wayland falls through to 3.
//  3. Wayland ext-idle-notify-v1 (KWin, Sway, Hyprland, niri, labwc, ...):
//     event based, precise to the notification timeout. Not on X11 sessions.
//  4. The X11 MIT-SCREEN-SAVER extension: precise. Only on real X11
//     sessions; under Wayland it would only see input sent to XWayland.
//  5. logind IdleHint/IdleSinceHint: coarse, and only trusted once the
//     desktop has been seen setting IdleHint (see logindIdle).
//
// A source that doesn't apply to the session returns ErrUnavailable
// (linuxSession.allows), so the order itself is fixed and the session can be
// learned after start (see sessionFromLogind).
var linuxIdleOrder = []string{srcMutter, srcScreenSaver, srcWayland, srcX11, srcLogind}

// linuxSession describes the graphical session.
type linuxSession struct {
	wayland bool   // a Wayland session (X11 data would only cover XWayland apps)
	x11     bool   // a plain X11 session we can query
	display string // the X display (":0") when x11
}

// known reports whether the session type has been determined.
func (s linuxSession) known() bool { return s.wayland || s.x11 }

// allows reports whether an idle source can apply to this session. The
// Wayland source is still tried while the type is unknown: without a
// compositor socket it fails at once.
func (s linuxSession) allows(src string) bool {
	switch src {
	case srcX11:
		return s.x11
	case srcWayland:
		return !s.x11
	}
	return true
}

// detectSession reads the session type from environment variables, looked up
// through getenv so tests can fake them.
//
// A systemd --user service only sees these when the desktop imports them into
// the user manager, which may happen after the service started. When they are
// missing the probe asks logind instead (sessionFromLogind).
func detectSession(getenv func(string) string) linuxSession {
	wayland := getenv("XDG_SESSION_TYPE") == "wayland" || getenv("WAYLAND_DISPLAY") != ""
	display := getenv("DISPLAY")
	if wayland || display == "" {
		return linuxSession{wayland: wayland}
	}
	return linuxSession{x11: true, display: display}
}

// sessionFromLogind builds the session from logind's Session.Type and
// Session.Display properties of the user's display session.
func sessionFromLogind(typ, display string) linuxSession {
	switch {
	case typ == "wayland":
		return linuxSession{wayland: true}
	case typ == "x11" && display != "":
		return linuxSession{x11: true, display: display}
	}
	return linuxSession{}
}

// graphicalSession reports whether a logind session of this Type is a
// desktop whose idle hint describes the person using it. Without a desktop,
// logind elects a text session ("tty", even a remote SSH login) as the
// user's Display, and its hint tracks a terminal the person may not be
// using at all.
func graphicalSession(typ string) bool {
	switch typ {
	case "x11", "wayland", "mir":
		return true
	}
	return false
}

// logindState decides what logind's idle hints are worth.
//
// logind's IdleHint is only as good as the desktop that sets it, and several
// never do: KDE Plasma (KDE bug 448942), and wlroots compositors without an
// idle daemon configured to call SetIdleHint. There IdleHint stays false
// forever, which would read as "always active". So logind counts as a
// working source only after IdleHint has been seen true in this session;
// until then it reports ErrUnavailable and the sensor records nothing.
//
// Even when trusted it is coarse: IdleSinceHint is when the desktop flipped
// the hint, which is the desktop's idle delay (often minutes) after the last
// input, so idle time is understated by that delay, and short idle periods
// read as 0.
type logindState struct {
	session string // D-Bus object path of the session the trust applies to
	trusted bool
}

// idle turns one reading of IdleHint/IdleSinceHint (realtime microseconds)
// for session into an idle duration.
func (l *logindState) idle(session string, hint bool, sinceUsec uint64, now time.Time) (time.Duration, error) {
	if session != l.session {
		l.session, l.trusted = session, false
	}
	if hint {
		l.trusted = true
	}
	if !l.trusted {
		return 0, fmt.Errorf("logind: this desktop has not set IdleHint yet: %w", ErrUnavailable)
	}
	if !hint || sinceUsec == 0 {
		return 0, nil
	}
	d := now.Sub(time.UnixMicro(int64(sinceUsec))) //nolint:gosec // realtime microseconds fit in int64 until year 294247
	return max(d, 0), nil
}
