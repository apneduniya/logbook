//go:build linux

package osprobe

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWlCodecRoundTrip(t *testing.T) {
	msg := new(wlBuild).uint(7).str("wl_seat").uint(1).message(2, 0)
	if len(msg)%4 != 0 {
		t.Fatalf("message not aligned: %d bytes", len(msg))
	}
	buf := make([]byte, wlMaxMessage)
	m, err := wlRead(bytes.NewReader(msg), buf)
	if err != nil || m.object != 2 || m.opcode != 0 {
		t.Fatalf("wlRead = %+v, %v", m, err)
	}
	a := wlArgs{b: m.args}
	if n, s, v := a.uint(), a.str(), a.uint(); a.err != nil || n != 7 || s != "wl_seat" || v != 1 {
		t.Fatalf("args = %d %q %d %v", n, s, v, a.err)
	}

	// Malformed input is rejected, never read out of bounds.
	bad := [][]byte{
		new(wlBuild).message(1, 0)[:4],                     // short header
		append(new(wlBuild).message(1, 0)[:4], 0, 0, 4, 0), // size 4 < header
		append(new(wlBuild).message(1, 0)[:4], 0, 0, 9, 0), // unaligned size
	}
	for i, b := range bad {
		if _, err := wlRead(bytes.NewReader(b), buf); err == nil {
			t.Errorf("bad[%d] accepted", i)
		}
	}
	for i, args := range [][]byte{
		{},                           // no length
		{9, 0, 0, 0, 'a', 0},         // length past the end
		{2, 0, 0, 0, 'a', 'b', 0, 0}, // missing NUL
	} {
		a := wlArgs{b: args}
		if a.str(); a.err == nil {
			t.Errorf("bad string %d accepted", i)
		}
	}
}

func TestWaylandSocketPath(t *testing.T) {
	tests := []struct {
		env  map[string]string
		uid  int
		want string
		ok   bool
	}{
		{map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "WAYLAND_DISPLAY": "wayland-1"}, 1000, "/run/user/1000/wayland-1", true},
		{map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}, 1000, "/run/user/1000/wayland-0", true},
		{map[string]string{"WAYLAND_DISPLAY": "/tmp/w"}, 1000, "/tmp/w", true},
		{map[string]string{}, 1001, "/run/user/1001/wayland-0", true},
		{map[string]string{}, -1, "", false},
	}
	for _, tt := range tests {
		got, err := waylandSocketPath(func(k string) string { return tt.env[k] }, tt.uid)
		if got != tt.want || (err == nil) != tt.ok {
			t.Errorf("waylandSocketPath(%v, %d) = %q, %v", tt.env, tt.uid, got, err)
		}
	}
}

// fakeCompositor speaks just enough Wayland to serve one client.
type fakeCompositor struct {
	t       *testing.T
	ln      net.Listener
	version uint32 // advertised ext_idle_notifier_v1 version; 0 = absent
	mu      sync.Mutex
	conn    net.Conn
	created chan [3]uint32 // opcode, timeout, seat of the notification request
}

func startCompositor(t *testing.T, version uint32) (*fakeCompositor, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wl") // short path: unix socket paths are limited to 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "wayland-0")
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCompositor{t: t, ln: ln, version: version, created: make(chan [3]uint32, 1)}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f, path
}

func (f *fakeCompositor) serve() {
	c, err := f.ln.Accept()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn = c
	f.mu.Unlock()
	buf := make([]byte, wlMaxMessage)
	for {
		m, err := wlRead(c, buf)
		if err != nil {
			return
		}
		a := wlArgs{b: m.args}
		switch {
		case m.object == wlDisplayID && m.opcode == wlDisplayGetRegistry:
			reg := a.uint()
			f.send(new(wlBuild).uint(1).str("wl_compositor").uint(6).message(reg, wlRegistryGlobal))
			f.send(new(wlBuild).uint(2).str("wl_seat").uint(9).message(reg, wlRegistryGlobal))
			if f.version > 0 {
				f.send(new(wlBuild).uint(3).str("ext_idle_notifier_v1").uint(f.version).message(reg, wlRegistryGlobal))
			}
		case m.object == wlDisplayID && m.opcode == wlDisplaySync:
			f.send(new(wlBuild).uint(0).message(a.uint(), wlCallbackDone))
		case m.object == wlRegistryID && m.opcode == wlRegistryBind:
			name, iface, version, id := a.uint(), a.str(), a.uint(), a.uint()
			if iface == "wl_seat" {
				// A seat event the client must skip.
				f.send(new(wlBuild).uint(3).message(id, 0))
			}
			if (name == 3) != (iface == "ext_idle_notifier_v1") || version == 0 {
				f.send(new(wlBuild).uint(wlDisplayID).uint(0).str("bad bind").message(wlDisplayID, wlDisplayError))
			}
		case m.object == wlNotifierID:
			_, timeout, seat := a.uint(), a.uint(), a.uint()
			f.created <- [3]uint32{uint32(m.opcode), timeout, seat}
		}
	}
}

func (f *fakeCompositor) send(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.conn.Write(b); err != nil {
		f.t.Logf("fake compositor write: %v", err)
	}
}

func (f *fakeCompositor) event(opcode uint16) { f.send(new(wlBuild).message(wlNotificationID, opcode)) }

func (f *fakeCompositor) hangUp() {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.conn.Close()
}

// waitFor polls cond: the reader goroutine applies events asynchronously.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWaylandIdleNotify(t *testing.T) {
	f, path := startCompositor(t, 2)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
	w := newWaylandIdle(func() (string, error) { return path, nil }, clock)
	defer w.close()

	// Connecting: the version-2 input-idle request, our timeout, our seat.
	if d, err := w.idle(t.Context()); err != nil || d != 0 {
		t.Fatalf("first idle = %v, %v", d, err)
	}
	if got := <-f.created; got != [3]uint32{extNotifierGetInputIdle, uint32(waylandNotifyTimeout / time.Millisecond), wlSeatID} {
		t.Fatalf("notification request = %v", got)
	}

	idle := func() time.Duration {
		d, err := w.idle(t.Context())
		if err != nil {
			t.Fatalf("idle: %v", err)
		}
		return d
	}
	f.event(extNotificationIdled)
	waitFor(t, "idled", func() bool { return idle() == waylandNotifyTimeout })
	advance(30 * time.Second)
	if d := idle(); d != 30*time.Second+waylandNotifyTimeout {
		t.Fatalf("idle after 30s = %v", d)
	}
	f.event(extNotificationResumed)
	waitFor(t, "resumed", func() bool { return idle() == 0 })

	// The compositor going away is reported, and the source then waits
	// before reconnecting.
	f.hangUp()
	waitFor(t, "disconnect", func() bool { _, err := w.idle(t.Context()); return err != nil })
	if _, err := w.idle(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reconnect not rate limited: %v", err)
	}
}

func TestWaylandVersion1UsesIdleNotification(t *testing.T) {
	f, path := startCompositor(t, 1)
	w := newWaylandIdle(func() (string, error) { return path, nil }, time.Now)
	defer w.close()
	if _, err := w.idle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := <-f.created; got[0] != extNotifierGetIdle {
		t.Fatalf("v1 compositor got opcode %d", got[0])
	}
}

func TestWaylandUnavailable(t *testing.T) {
	t.Run("no ext-idle-notify global", func(t *testing.T) {
		_, path := startCompositor(t, 0)
		w := newWaylandIdle(func() (string, error) { return path, nil }, time.Now)
		defer w.close()
		if _, err := w.idle(t.Context()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("no socket", func(t *testing.T) {
		w := newWaylandIdle(func() (string, error) { return filepath.Join(t.TempDir(), "none"), nil }, time.Now)
		if _, err := w.idle(t.Context()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("silent compositor times out", func(t *testing.T) {
		dir, err := os.MkdirTemp("", "wl")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "w")
		ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		w := newWaylandIdle(func() (string, error) { return path, nil }, time.Now)
		start := time.Now()
		if _, err := w.idle(t.Context()); err == nil {
			t.Fatal("silent compositor accepted")
		}
		if el := time.Since(start); el > 3*callTimeout {
			t.Fatalf("handshake took %v", el)
		}
	})
}

func TestBoundedDial(t *testing.T) {
	var b boundedDial[int]
	release := make(chan struct{})
	starts := 0
	slow := func() (int, error) { starts++; <-release; return 42, nil }

	if _, err := b.dial(t.Context(), 20*time.Millisecond, slow); err == nil {
		t.Fatal("slow connect did not time out")
	}
	// A second call waits on the same attempt, not a new one.
	if _, err := b.dial(t.Context(), 20*time.Millisecond, slow); err == nil {
		t.Fatal("still-pending connect returned")
	}
	close(release)
	if v, err := b.dial(t.Context(), time.Second, slow); err != nil || v != 42 {
		t.Fatalf("late result = %v, %v", v, err)
	}
	if starts != 1 {
		t.Fatalf("connect started %d times", starts)
	}

	// Abandoning a pending connect releases its late result.
	released := make(chan int, 1)
	hold := make(chan struct{})
	_, _ = b.dial(t.Context(), time.Millisecond, func() (int, error) { <-hold; return 7, nil })
	b.abandon(func(v int) { released <- v })
	close(hold)
	if v := <-released; v != 7 {
		t.Fatalf("released %d", v)
	}
}

func TestFocusPID(t *testing.T) {
	tests := []struct {
		name string
		w    x11Window
		want int
	}{
		{"x-resource host pid wins over sandbox pid (flatpak)", x11Window{xresOK: true, xresPID: 4242, wmPID: 2, machine: "box"}, 4242},
		{"x-resource knows no pid (remote client)", x11Window{xresOK: true, wmPID: 999, machine: "box"}, 0},
		{"no x-resource, local client", x11Window{wmPID: 1234, machine: "box"}, 1234},
		{"no x-resource, remote client", x11Window{wmPID: 1234, machine: "server"}, 0},
		{"no x-resource, no machine property", x11Window{wmPID: 1234}, 1234},
		{"nothing known", x11Window{}, 0},
	}
	for _, tt := range tests {
		if got := focusPID(tt.w, "box"); got != tt.want {
			t.Errorf("%s: focusPID = %d, want %d", tt.name, got, tt.want)
		}
	}
}
