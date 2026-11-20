package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/install/keycheck"
	"github.com/apneduniya/logbook/internal/agent/install/selfinstall"
	"github.com/apneduniya/logbook/internal/agent/install/wakacfg"
)

const (
	key       = "3f1e2d4c-0000-4000-8000-000000000000"
	hackKey   = "11111111-1111-4111-8111-111111111111"
	hackURL   = "https://hackatime.hackclub.com/api/hackatime/v1"
	origWaka  = "[settings]\napi_url = " + hackURL + "\napi_key = " + hackKey + "\n"
	binTarget = "/home/u/.local/bin/logbook"
)

// log records every fake call in order, shared by all fakes of one test.
type log struct{ calls []string }

func (l *log) add(s string) { l.calls = append(l.calls, s) }

type fakeBinary struct {
	log       *log
	installed bool
}

func (f *fakeBinary) Path() string { return binTarget }
func (f *fakeBinary) Install(string) (selfinstall.Result, error) {
	f.log.add("binary.install")
	changed := !f.installed
	f.installed = true
	return selfinstall.Result{Path: binTarget, Changed: changed}, nil
}
func (f *fakeBinary) Rollback(r selfinstall.Result) error {
	f.log.add("binary.rollback")
	if r.Changed {
		f.installed = false
	}
	return nil
}
func (f *fakeBinary) Finish(selfinstall.Result) error { f.log.add("binary.finish"); return nil }
func (f *fakeBinary) Uninstall() (bool, error) {
	f.log.add("binary.uninstall")
	was := f.installed
	f.installed = false
	return was, nil
}

type fakeAutostart struct {
	log       *log
	installed bool
	startErr  error
}

func (f *fakeAutostart) Path() string { return "/autostart/entry" }
func (f *fakeAutostart) Install(bin string) error {
	f.log.add("autostart.install " + bin)
	f.installed = true
	return nil
}
func (f *fakeAutostart) Uninstall() error {
	f.log.add("autostart.uninstall")
	f.installed = false
	return nil
}
func (f *fakeAutostart) Installed() (bool, error) { return f.installed, nil }
func (f *fakeAutostart) Start() error             { f.log.add("autostart.start"); return f.startErr }

type fakeHarness struct {
	log        *log
	name       string
	detected   bool
	installed  bool
	installErr error
}

func (f *fakeHarness) Name() string { return f.name }
func (f *fakeHarness) Detect() bool { return f.detected }
func (f *fakeHarness) Install(bin string) error {
	f.log.add("hook.install " + f.name + " " + bin)
	if f.installErr != nil {
		return f.installErr
	}
	f.installed = true
	return nil
}
func (f *fakeHarness) Uninstall() error {
	f.log.add("hook.uninstall " + f.name)
	f.installed = false
	return nil
}
func (f *fakeHarness) Installed() (bool, error) { return f.installed, nil }

type fakeKeys struct{ err error }

func (f fakeKeys) Check(context.Context, string, string) error { return f.err }

type env struct {
	log     *log
	svc     *Service
	data    string
	cfgPath string
	out     *bytes.Buffer
	bin     *fakeBinary
	auto    *fakeAutostart
	claude  *fakeHarness
	codex   *fakeHarness
	cursor  *fakeHarness
}

func newEnv(t *testing.T, keysErr error) *env {
	t.Helper()
	l := &log{}
	e := &env{
		log:     l,
		data:    filepath.Join(t.TempDir(), "logbook"),
		cfgPath: filepath.Join(t.TempDir(), ".wakatime.cfg"),
		out:     &bytes.Buffer{},
		bin:     &fakeBinary{log: l},
		auto:    &fakeAutostart{log: l},
		claude:  &fakeHarness{log: l, name: "claude-code", detected: true},
		codex:   &fakeHarness{log: l, name: "codex", detected: true},
		cursor:  &fakeHarness{log: l, name: "cursor"},
	}
	if err := os.MkdirAll(e.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.cfgPath, []byte(origWaka), 0o600); err != nil {
		t.Fatal(err)
	}
	e.svc = New(Deps{
		DataDir:   e.data,
		Binary:    e.bin,
		Waka:      wakacfg.New(e.cfgPath, e.data, ""),
		Autostart: e.auto,
		Harnesses: []HarnessInstaller{e.claude, e.codex, e.cursor},
		Keys:      fakeKeys{err: keysErr},
		Out:       e.out,
	})
	return e
}

func (e *env) waka(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallHappyPathAndIdempotent(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if err := e.svc.Install(ctx, Options{Key: key, Harness: "all", SourceBinary: "/tmp/logbook"}); err != nil {
		t.Fatalf("Install() = %v\n%s", err, e.out)
	}
	want := []string{
		"binary.install",
		"hook.install claude-code " + binTarget,
		"hook.install codex " + binTarget,
		"autostart.install " + binTarget,
		"autostart.start",
		"binary.finish",
	}
	if !reflect.DeepEqual(e.log.calls, want) {
		t.Fatalf("calls = %q\nwant %q", e.log.calls, want)
	}
	cfg, err := config.Load(e.data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != key || cfg.ServerURL != config.DefaultServerURL {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Hackatime != (config.Hackatime{URL: hackURL, APIKey: hackKey, Mirror: true}) {
		t.Fatalf("mirror = %+v", cfg.Hackatime)
	}
	if w := e.waka(t); !strings.Contains(w, "api_url = http://"+cfg.LocalAddr+"/api/v1") || !strings.Contains(w, "api_key = "+key) {
		t.Fatalf(".wakatime.cfg = %s", w)
	}
	out := e.out.String()
	for _, secret := range []string{key, hackKey, cfg.LocalToken} {
		if strings.Contains(out, secret) {
			t.Fatalf("output leaks a secret:\n%s", out)
		}
	}
	if !strings.Contains(out, "cursor not found, skipped") || !strings.Contains(out, "Hackatime") {
		t.Fatalf("output:\n%s", out)
	}

	// Second run: same token, nothing new, no rollback.
	e.log.calls, e.out = nil, &bytes.Buffer{}
	e.svc.d.Out = e.out
	if err := e.svc.Install(ctx, Options{Key: key, SourceBinary: "/tmp/logbook"}); err != nil {
		t.Fatal(err)
	}
	cfg2, err := config.Load(e.data)
	if err != nil || cfg2 != cfg {
		t.Fatalf("config changed on reinstall: %+v, %v", cfg2, err)
	}
	if !strings.Contains(e.out.String(), "already points at Logbook") || strings.Contains(e.out.String(), "undo") {
		t.Fatalf("reinstall output:\n%s", e.out)
	}
}

func TestInstallRollsBackOnMidwayFailure(t *testing.T) {
	e := newEnv(t, nil)
	e.auto.startErr = errors.New("launchctl exploded")
	err := e.svc.Install(context.Background(), Options{Key: key, SourceBinary: "/tmp/logbook"})
	if err == nil || !strings.Contains(err.Error(), "launchctl exploded") {
		t.Fatalf("Install() = %v", err)
	}
	wantTail := []string{
		"autostart.start",
		"autostart.uninstall",
		"hook.uninstall codex",
		"hook.uninstall claude-code",
		"binary.rollback",
	}
	if got := e.log.calls[len(e.log.calls)-len(wantTail):]; !reflect.DeepEqual(got, wantTail) {
		t.Fatalf("rollback calls = %q\nwant %q", got, wantTail)
	}
	if e.waka(t) != origWaka {
		t.Fatalf(".wakatime.cfg not restored:\n%s", e.waka(t))
	}
	if _, err := config.Load(e.data); !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("config not removed: %v", err)
	}
	if e.bin.installed || e.auto.installed || e.claude.installed || e.codex.installed {
		t.Fatal("something was left installed")
	}
}

func TestInstallRollbackKeepsEarlierInstall(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if err := e.svc.Install(ctx, Options{Key: key, SourceBinary: "/tmp/logbook"}); err != nil {
		t.Fatal(err)
	}
	before := e.waka(t)
	e.auto.startErr = errors.New("boom")
	if err := e.svc.Install(ctx, Options{Key: key, SourceBinary: "/tmp/logbook"}); err == nil {
		t.Fatal("Install() = nil")
	}
	// A failed reinstall must not tear down the working install.
	if e.waka(t) != before || !e.auto.installed || !e.claude.installed {
		t.Fatal("failed reinstall undid the earlier install")
	}
	if _, err := config.Load(e.data); err != nil {
		t.Fatalf("config lost: %v", err)
	}
}

func TestInstallRejectsBeforeChangingAnything(t *testing.T) {
	tests := []struct {
		name string
		keys error
		opts Options
	}{
		{"bad key format", nil, Options{Key: "nope"}},
		{"server rejects key", keycheck.ErrRejected, Options{Key: key}},
		{"unknown harness", nil, Options{Key: key, Harness: "vim"}},
		{"bad mirror url", nil, Options{Key: key, MirrorURL: "nope"}},
		{"bad server url", nil, Options{Key: key, ServerURL: "ftp://x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.keys)
			if err := e.svc.Install(context.Background(), tt.opts); err == nil {
				t.Fatal("Install() = nil")
			}
			if len(e.log.calls) != 0 || e.waka(t) != origWaka {
				t.Fatalf("changed things: %q", e.log.calls)
			}
			if _, err := config.Load(e.data); !errors.Is(err, config.ErrNotConfigured) {
				t.Fatalf("config written: %v", err)
			}
		})
	}
}

func TestHarnessSelectionAndFailure(t *testing.T) {
	e := newEnv(t, nil)
	e.codex.installErr = errors.New("settings.json is not JSON")
	if err := e.svc.Install(context.Background(), Options{Key: key, Harness: "codex, claude-code,codex"}); err != nil {
		t.Fatalf("a harness failure must not fail the install: %v", err)
	}
	if !e.claude.installed || e.codex.installed {
		t.Fatal("wrong harnesses installed")
	}
	if !strings.Contains(e.out.String(), "codex: FAILED") {
		t.Fatalf("failure not reported:\n%s", e.out)
	}

	e2 := newEnv(t, nil)
	if err := e2.svc.Install(context.Background(), Options{Key: key, Harness: "none"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range e2.log.calls {
		if strings.HasPrefix(c, "hook.") {
			t.Fatalf("--harness none ran %q", c)
		}
	}
}

func TestReinstallKeepsMirrorChoice(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if err := e.svc.Install(ctx, Options{Key: key, NoMirror: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Install(ctx, Options{Key: key}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(e.data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hackatime.Mirror || cfg.Hackatime.APIKey != hackKey {
		t.Fatalf("mirror = %+v, want saved but off", cfg.Hackatime)
	}
	if err := e.svc.Install(ctx, Options{Key: key, MirrorURL: "https://wakapi.example/api"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = config.Load(e.data); cfg.Hackatime != (config.Hackatime{URL: "https://wakapi.example/api", APIKey: hackKey, Mirror: true}) {
		t.Fatalf("mirror = %+v", cfg.Hackatime)
	}
}

func TestUninstall(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if err := e.svc.Install(ctx, Options{Key: key}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Uninstall(ctx, UninstallOptions{Purge: true}); !errors.Is(err, ErrPurgeNeedsYes) {
		t.Fatalf("Uninstall(purge) = %v", err)
	}
	e.log.calls = nil
	if err := e.svc.Uninstall(ctx, UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"autostart.uninstall", "hook.uninstall claude-code", "hook.uninstall codex", "binary.uninstall"}
	if !reflect.DeepEqual(e.log.calls, want) {
		t.Fatalf("calls = %q\nwant %q", e.log.calls, want)
	}
	if e.waka(t) != origWaka {
		t.Fatalf(".wakatime.cfg not restored:\n%s", e.waka(t))
	}
	if _, err := config.Load(e.data); err != nil {
		t.Fatalf("config should stay without --purge: %v", err)
	}
	// Idempotent.
	if err := e.svc.Uninstall(ctx, UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Uninstall(ctx, UninstallOptions{Purge: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.data); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("data dir not purged")
	}
}

func TestPurgeSavesUnusedBackup(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if err := e.svc.Install(ctx, Options{Key: key}); err != nil {
		t.Fatal(err)
	}
	// The user edits the file, so uninstall restores keys only and the
	// backup would otherwise be purged.
	if err := os.WriteFile(e.cfgPath, []byte(e.waka(t)+"debug = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Uninstall(ctx, UninstallOptions{Purge: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.cfgPath + ".logbook-backup")
	if err != nil || string(b) != origWaka {
		t.Fatalf("backup = %q, %v", b, err)
	}
}

func TestCheckPurgeable(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	other := t.TempDir()
	marked := t.TempDir()
	if err := os.WriteFile(config.Path(marked), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		dir string
		ok  bool
	}{
		{string(filepath.Separator), false},
		{home, false},
		{other, false},
		{marked, true},
		{filepath.Join(other, "logbook"), true},
	}
	for _, tt := range tests {
		if err := checkPurgeable(tt.dir); (err == nil) != tt.ok {
			t.Errorf("checkPurgeable(%q) = %v, want ok=%v", tt.dir, err, tt.ok)
		}
	}
}
