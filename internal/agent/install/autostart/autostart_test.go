package autostart

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s mismatch:\n%s\nwant:\n%s", name, got, want)
	}
}

func TestGoldenFiles(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
	}{
		{"launchd.plist.golden", LaunchdPlist("/Users/ada/Library/Application Support/Logbook/bin/logbook", "/Users/ada/Library/Application Support/logbook/launchd.log")},
		{"launchd-escaped.plist.golden", LaunchdPlist("/Users/a&b/<x>/logbook", "/tmp/l.log")},
		{"logbook.service.golden", SystemdUnit("/home/ada/.local/bin/logbook", true)},
		{"logbook-headless.service.golden", SystemdUnit("/home/ada/.local/bin/logbook", false)},
		{"logbook-escaped.service.golden", SystemdUnit(`/home/a b/100%/$x/"q"\logbook`, true)},
		{"logbook.desktop.golden", DesktopEntry("/home/ada/.local/bin/logbook")},
		{"logbook-escaped.desktop.golden", DesktopEntry(`/home/a b/100%/$x/"q"\logbook`)},
		{"windows-run.golden", []byte(WindowsRunValue(`C:\Users\Ada Lovelace\AppData\Local\Programs\Logbook\logbook.exe`))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { golden(t, tt.name, tt.got) })
	}
}

func TestLaunchd(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	r := &fakeRunner{fail: map[string]error{"launchctl print": errFake}}
	l := NewLaunchd(home, data, 501, r)

	if err := l.Install("/bin/logbook"); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"launchctl print gui/501/dev.boogles.logbook",
		"launchctl bootstrap gui/501 " + l.Path(),
	}
	if !reflect.DeepEqual(r.calls, wantCalls) {
		t.Fatalf("calls = %q, want %q", r.calls, wantCalls)
	}
	if ok, err := l.Installed(); !ok || err != nil {
		t.Fatalf("Installed() = %v, %v", ok, err)
	}

	// Loaded and unchanged: nothing to do.
	r.fail, r.calls = nil, nil
	if err := l.Install("/bin/logbook"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, []string{"launchctl print gui/501/dev.boogles.logbook"}) {
		t.Fatalf("idempotent install ran %q", r.calls)
	}

	// Loaded and changed: unload, wait until launchd has really dropped
	// the job (it is still listed for a moment after bootout), then load.
	r.calls = nil
	var slept []time.Duration
	l.sleep = func(d time.Duration) { slept = append(slept, d) }
	printsAfterBootout := -1
	r.fn = func(c string) error {
		switch {
		case strings.HasPrefix(c, "launchctl bootout"):
			printsAfterBootout = 0
		case strings.HasPrefix(c, "launchctl print") && printsAfterBootout >= 0:
			printsAfterBootout++
			if printsAfterBootout > 2 {
				return errFake
			}
		}
		return nil
	}
	if err := l.Install("/other/logbook"); err != nil {
		t.Fatal(err)
	}
	wantCalls = []string{
		"launchctl print gui/501/dev.boogles.logbook",
		"launchctl bootout gui/501/dev.boogles.logbook",
		"launchctl print gui/501/dev.boogles.logbook",
		"launchctl print gui/501/dev.boogles.logbook",
		"launchctl print gui/501/dev.boogles.logbook",
		"launchctl bootstrap gui/501 " + l.Path(),
	}
	if !reflect.DeepEqual(r.calls, wantCalls) {
		t.Fatalf("calls = %q, want %q", r.calls, wantCalls)
	}
	if len(slept) != 2 {
		t.Fatalf("slept %v, want two polls", slept)
	}
	r.fn = nil

	// A job that never goes away fails after a bounded wait instead of
	// sending a bootstrap that launchd would reject.
	r.calls, slept = nil, nil
	if err := l.Install("/third/logbook"); err == nil {
		t.Fatal("Install() = nil while the old job stayed loaded")
	}
	if len(slept) != bootoutPolls {
		t.Fatalf("slept %d times, want %d", len(slept), bootoutPolls)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "launchctl bootstrap") {
			t.Fatal("bootstrap sent while the old job was still loaded")
		}
	}
	// Put the expected plist back for the rest of the test.
	r.fail = map[string]error{"launchctl print": errFake}
	if err := l.Install("/other/logbook"); err != nil {
		t.Fatal(err)
	}
	r.fail = nil

	r.calls = nil
	if err := l.Start(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, []string{"launchctl kickstart -k gui/501/dev.boogles.logbook"}) {
		t.Fatalf("Start ran %q", r.calls)
	}

	r.calls = nil
	if err := l.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, []string{
		"launchctl print gui/501/dev.boogles.logbook",
		"launchctl bootout gui/501/dev.boogles.logbook",
	}) {
		t.Fatalf("Uninstall ran %q", r.calls)
	}
	if ok, _ := l.Installed(); ok {
		t.Fatal("plist still present")
	}
}

func TestLinuxSystemd(t *testing.T) {
	cfg := t.TempDir()
	r := &fakeRunner{}
	s := &fakeSpawner{}
	l := NewLinux(cfg, r, s)
	// A stale XDG entry from an earlier fallback install is removed.
	desktop := filepath.Join(cfg, "autostart", "logbook.desktop")
	if err := os.MkdirAll(filepath.Dir(desktop), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desktop, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := l.Install("/home/u/.local/bin/logbook"); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(cfg, "systemd", "user", "logbook.service")
	if b, err := os.ReadFile(unit); err != nil || string(b) != string(SystemdUnit("/home/u/.local/bin/logbook", true)) {
		t.Fatalf("unit = %q, %v", b, err)
	}
	if _, err := os.Stat(desktop); !os.IsNotExist(err) {
		t.Fatal("stale desktop entry not removed")
	}
	if err := l.Start(); err != nil {
		t.Fatal(err)
	}
	if err := l.Uninstall(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user show-environment",
		"systemctl --user is-active --quiet graphical-session.target",
		"systemctl --user daemon-reload",
		"systemctl --user reenable logbook.service",
		"systemctl --user show-environment",
		"systemctl --user restart logbook.service",
		"systemctl --user show-environment",
		"systemctl --user disable --now logbook.service",
		"systemctl --user show-environment",
		"systemctl --user daemon-reload",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q\nwant %q", r.calls, want)
	}
	if len(s.spawned) != 0 {
		t.Fatalf("spawned %q under systemd", s.spawned)
	}
	if ok, _ := l.Installed(); ok {
		t.Fatal("still installed")
	}
}

func TestLinuxSystemdWithoutGraphicalSession(t *testing.T) {
	cfg := t.TempDir()
	r := &fakeRunner{fail: map[string]error{"systemctl --user is-active": exitErr(3)}}
	l := NewLinux(cfg, r, &fakeSpawner{})
	if err := l.Install("/home/u/.local/bin/logbook"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg, "systemd", "user", "logbook.service"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(SystemdUnit("/home/u/.local/bin/logbook", false)) {
		t.Fatalf("unit = %s", b)
	}
	if !strings.Contains(string(b), "WantedBy=default.target") || strings.Contains(string(b), "graphical-session") {
		t.Fatalf("headless unit binds to a graphical session:\n%s", b)
	}
}

func TestNeedsRelaunch(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		procs int
		want  bool
	}{
		{"login: own visible console", "", 1, true},
		{"started from a terminal", "", 2, false},
		{"no console", "", 0, false},
		{"already detached", "1", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsRelaunch(tt.env, tt.procs); got != tt.want {
				t.Fatalf("needsRelaunch(%q, %d) = %v, want %v", tt.env, tt.procs, got, tt.want)
			}
		})
	}
}

func TestLinuxFallback(t *testing.T) {
	cfg := t.TempDir()
	r := &fakeRunner{fail: map[string]error{
		"systemctl": errFake,
		"pkill":     exitErr(1), // nothing to stop
	}}
	s := &fakeSpawner{}
	l := NewLinux(cfg, r, s)
	if err := l.Install("/home/u/.local/bin/logbook"); err != nil {
		t.Fatal(err)
	}
	if l.Path() != filepath.Join(cfg, "autostart", "logbook.desktop") {
		t.Fatalf("Path() = %q", l.Path())
	}
	if ok, err := l.Installed(); !ok || err != nil {
		t.Fatalf("Installed() = %v, %v", ok, err)
	}
	if err := l.Start(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.spawned, []string{"/home/u/.local/bin/logbook run"}) {
		t.Fatalf("spawned %q", s.spawned)
	}
	if err := l.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := l.Installed(); ok {
		t.Fatal("still installed")
	}

	r.fail["pkill"] = exitErr(3) // a real pkill error
	if err := l.Install("/x/logbook"); err != nil {
		t.Fatal(err)
	}
	if err := l.Start(); err == nil {
		t.Fatal("Start() ignored a pkill failure")
	}
}

func TestLinuxStartBeforeInstall(t *testing.T) {
	l := NewLinux(t.TempDir(), &fakeRunner{fail: map[string]error{"systemctl": errFake}}, &fakeSpawner{})
	if err := l.Start(); err == nil {
		t.Fatal("Start() = nil before Install")
	}
}

func TestWindows(t *testing.T) {
	key := &fakeRunKey{values: map[string]string{"Other": "keep"}}
	r := &fakeRunner{}
	s := &fakeSpawner{}
	w := NewWindows(key, r, s, 4242)
	bin := `C:\Users\Ada\AppData\Local\Programs\Logbook\logbook.exe`

	if err := w.Install(bin); err != nil {
		t.Fatal(err)
	}
	if err := w.Install(bin); err != nil {
		t.Fatal(err)
	}
	if key.values[RunValueName] != WindowsRunValue(bin) || key.values["Other"] != "keep" {
		t.Fatalf("Run key = %v", key.values)
	}
	if ok, _ := w.Installed(); !ok {
		t.Fatal("not installed")
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, []string{`taskkill /F /IM logbook.exe /FI PID ne 4242`}) {
		t.Fatalf("calls = %q", r.calls)
	}
	if !reflect.DeepEqual(s.spawned, []string{bin + " run"}) {
		t.Fatalf("spawned = %q", s.spawned)
	}
	if err := w.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, ok := key.values[RunValueName]; ok || key.values["Other"] != "keep" {
		t.Fatalf("Run key after uninstall = %v", key.values)
	}
	if err := w.Uninstall(); err != nil {
		t.Fatalf("second Uninstall() = %v", err)
	}
}
