//go:build linux

package osprobe

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"
)

func TestDetectSession(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want linuxSession
	}{
		{"x11", map[string]string{"XDG_SESSION_TYPE": "x11", "DISPLAY": ":0"}, linuxSession{x11: true, display: ":0"}},
		{"x11 without session type", map[string]string{"DISPLAY": ":1"}, linuxSession{x11: true, display: ":1"}},
		{"wayland with xwayland display", map[string]string{"XDG_SESSION_TYPE": "wayland", "DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0"},
			linuxSession{wayland: true}},
		{"wayland display only", map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":1"}, linuxSession{wayland: true}},
		{"headless or not yet imported", map[string]string{}, linuxSession{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectSession(func(k string) string { return tt.env[k] }); got != tt.want {
				t.Fatalf("detectSession = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSessionFromLogind(t *testing.T) {
	tests := []struct {
		typ, display string
		want         linuxSession
	}{
		{"wayland", "", linuxSession{wayland: true}},
		{"wayland", ":0", linuxSession{wayland: true}}, // XWayland display; still a Wayland session
		{"x11", ":0", linuxSession{x11: true, display: ":0"}},
		{"x11", "", linuxSession{}},
		{"tty", "", linuxSession{}},
		{"", "", linuxSession{}},
	}
	for _, tt := range tests {
		if got := sessionFromLogind(tt.typ, tt.display); got != tt.want {
			t.Errorf("sessionFromLogind(%q, %q) = %+v, want %+v", tt.typ, tt.display, got, tt.want)
		}
	}
}

// TestGraphicalSession: logind's Display can be a text session (an SSH
// login on a machine with no desktop); only desktops count.
func TestGraphicalSession(t *testing.T) {
	for typ, want := range map[string]bool{
		"x11": true, "wayland": true, "mir": true,
		"tty": false, "unspecified": false, "web": false, "": false,
	} {
		if got := graphicalSession(typ); got != want {
			t.Errorf("graphicalSession(%q) = %v, want %v", typ, got, want)
		}
	}
}

// TestSourceSelection is the Linux backend selection with fakes: the fixed
// order, filtered by what the session allows, picks the first working source.
func TestSourceSelection(t *testing.T) {
	x11 := linuxSession{x11: true, display: ":0"}
	wayland := linuxSession{wayland: true}
	unknown := linuxSession{}
	tests := []struct {
		name    string
		session linuxSession
		working []string // sources that answer
		want    string
	}{
		{"gnome wayland", wayland, []string{srcMutter, srcWayland, srcLogind}, srcMutter},
		{"plasma wayland: screensaver NotSupported", wayland, []string{srcWayland, srcLogind}, srcWayland},
		{"plasma x11", x11, []string{srcScreenSaver, srcX11, srcLogind}, srcScreenSaver},
		{"xfce x11", x11, []string{srcX11, srcLogind}, srcX11},
		{"x11 never uses wayland", x11, []string{srcWayland, srcLogind}, srcLogind},
		{"wayland never uses x11", wayland, []string{srcX11, srcLogind}, srcLogind},
		{"unknown session tries wayland", unknown, []string{srcWayland, srcX11}, srcWayland},
		{"nothing works", wayland, nil, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sources []idleSource
			for _, name := range linuxIdleOrder {
				ok := slices.Contains(tt.working, name) && tt.session.allows(name)
				sources = append(sources, idleSource{name, func(context.Context) (time.Duration, error) {
					if ok {
						return time.Second, nil
					}
					return 0, ErrUnavailable
				}})
			}
			c := newIdleChain(sources, time.Now, slog.New(slog.DiscardHandler))
			_, _ = c.Idle(t.Context())
			if got := c.Current(); got != tt.want {
				t.Fatalf("selected %s, want %s", got, tt.want)
			}
		})
	}
}

func TestLogindState(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	since := uint64(now.Add(-7 * time.Minute).UnixMicro()) //nolint:gosec // positive test time

	// A desktop that never sets IdleHint (Plasma, bare wlroots): IdleHint is
	// false forever. That must read as unknown, never as "active".
	var l logindState
	for range 5 {
		if d, err := l.idle("/s/1", false, 0, now); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("untrusted hint=false: %v, %v; want ErrUnavailable", d, err)
		}
	}

	// Once the desktop sets it, logind is trusted for this session.
	if d, err := l.idle("/s/1", true, since, now); err != nil || d != 7*time.Minute {
		t.Fatalf("hint=true: %v, %v", d, err)
	}
	if d, err := l.idle("/s/1", false, since, now); err != nil || d != 0 {
		t.Fatalf("trusted hint=false: %v, %v; want active", d, err)
	}
	// Idle with an unknown start time reads as just idle.
	if d, err := l.idle("/s/1", true, 0, now); err != nil || d != 0 {
		t.Fatalf("hint=true since=0: %v, %v", d, err)
	}
	// A new session starts untrusted again.
	if _, err := l.idle("/s/2", false, 0, now); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("new session trusted: %v", err)
	}
	// A clock behind IdleSinceHint never gives negative idle.
	if d, _ := l.idle("/s/2", true, uint64(now.Add(time.Minute).UnixMicro()), now); d != 0 { //nolint:gosec // positive test time
		t.Fatalf("future since: %v", d)
	}
}

// TestLogindNeverIdleIsUnavailable: the chain's last resort is logind; when
// it always says "not idle", the chain reports unavailable (the sensor then
// emits no sample) instead of settling on it with 0 s idle.
func TestLogindNeverIdleIsUnavailable(t *testing.T) {
	var l logindState
	errDown := errors.New("NotSupported")
	c := newIdleChain([]idleSource{
		{srcScreenSaver, func(context.Context) (time.Duration, error) { return 0, errDown }},
		{srcLogind, func(context.Context) (time.Duration, error) {
			return l.idle("/s/1", false, 0, time.Now())
		}},
	}, time.Now, slog.New(slog.DiscardHandler))
	for range 3 {
		if d, err := c.Idle(t.Context()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Idle() = %v, %v; want ErrUnavailable", d, err)
		}
	}
	if c.Current() != "none" {
		t.Fatalf("current = %s, want none", c.Current())
	}
}
