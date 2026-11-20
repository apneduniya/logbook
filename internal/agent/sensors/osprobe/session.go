package osprobe

// Names of the Linux idle sources, in the order they are tried.
const (
	srcMutter      = "gnome-mutter-idlemonitor"
	srcScreenSaver = "freedesktop-screensaver"
	srcX11         = "x11-screensaver-ext"
	srcLogind      = "logind-idlehint"
)

// linuxSession describes the graphical session from its environment.
type linuxSession struct {
	wayland bool // a Wayland session (X11 data would only cover XWayland apps)
	x11     bool // a plain X11 session we can query through DISPLAY
}

// detectSession reads the session type from environment variables, looked up
// through getenv so tests can fake them.
//
// systemd --user services only see these variables when the desktop imports
// them (GNOME, KDE and most others do, via dbus-update-activation-environment).
func detectSession(getenv func(string) string) linuxSession {
	wayland := getenv("XDG_SESSION_TYPE") == "wayland" || getenv("WAYLAND_DISPLAY") != ""
	return linuxSession{
		wayland: wayland,
		x11:     !wayland && getenv("DISPLAY") != "",
	}
}

// linuxIdleOrder returns the idle sources to try, best first:
//
//  1. Mutter's IdleMonitor (GNOME, X11 and Wayland), precise;
//  2. org.freedesktop.ScreenSaver GetSessionIdleTime (KDE Plasma and others), precise;
//  3. the X11 MIT-SCREEN-SAVER extension, precise, only on real X11 sessions:
//     under Wayland it would only see input sent to XWayland windows;
//  4. logind's IdleHint/IdleSinceHint, coarse: it only changes when the
//     desktop marks the session idle (often after several minutes), and some
//     desktops never set it, so it is the last resort.
//
// X11 comes before logind (ahead of the order in the original brief) because
// a desktop that never sets IdleHint would otherwise report "active" forever.
func linuxIdleOrder(s linuxSession) []string {
	order := []string{srcMutter, srcScreenSaver}
	if s.x11 {
		order = append(order, srcX11)
	}
	return append(order, srcLogind)
}
