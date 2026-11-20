package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/install/keycheck"
	"github.com/apneduniya/logbook/internal/agent/install/selfinstall"
	"github.com/apneduniya/logbook/internal/agent/install/wakacfg"
)

// HarnessInstaller installs Logbook's hook into one AI harness. The
// harness module implements one per harness and lists them in
// harness.All().
type HarnessInstaller interface {
	Name() string
	// Detect reports whether the harness is installed on this machine.
	Detect() bool
	// Install adds hooks that run `<binPath> hook <harness> <event>`,
	// merging with the user's settings and backing them up first.
	Install(binPath string) error
	Uninstall() error
	Installed() (bool, error)
}

// Autostart starts the daemon at login (see package autostart).
type Autostart interface {
	Path() string
	Install(binPath string) error
	Uninstall() error
	Installed() (bool, error)
	Start() error
}

// Binary places the agent binary (see package selfinstall).
type Binary interface {
	Path() string
	Install(src string) (selfinstall.Result, error)
	Rollback(r selfinstall.Result) error
	Finish(r selfinstall.Result) error
	Uninstall() (bool, error)
}

// WakaConfig redirects .wakatime.cfg (see package wakacfg).
type WakaConfig interface {
	Path() string
	Install(localURL, key string) (wakacfg.InstallResult, error)
	Uninstall() (wakacfg.UninstallResult, error)
	// Revert undoes one Install call, for rollback.
	Revert(res wakacfg.InstallResult) error
}

// KeyChecker asks the Logbook server whether an API key is valid.
type KeyChecker interface {
	Check(ctx context.Context, serverURL, key string) error
}

// Deps are the step implementations. The command builds the real ones;
// tests pass fakes.
type Deps struct {
	DataDir   string
	Binary    Binary
	Waka      WakaConfig
	Autostart Autostart
	Harnesses []HarnessInstaller
	Keys      KeyChecker
	// Out receives one line per change, for the user.
	Out io.Writer
}

// Service runs install and uninstall.
type Service struct{ d Deps }

// New returns a Service.
func New(d Deps) *Service { return &Service{d: d} }

// Options are the `logbook install` flags.
type Options struct {
	Key       string
	ServerURL string // "" keeps the configured server (default config.DefaultServerURL)
	NoMirror  bool
	MirrorURL string // overrides where heartbeats are mirrored
	// Harness is "all" (every detected harness, the default), "none", or a
	// comma-separated list of harness names.
	Harness string
	// SourceBinary is the running executable, copied into place.
	SourceBinary string
}

// undo reverses one completed step.
type undo struct {
	name string
	fn   func() error
}

// Install runs every step in order. When a step fails, the steps already
// done are undone in reverse order where that is safe, and the error says
// what failed. Running it again with the same options changes nothing.
func (s *Service) Install(ctx context.Context, o Options) (err error) {
	key, err := keycheck.Normalize(o.Key)
	if err != nil {
		return err
	}
	selected, err := s.selectHarnesses(o.Harness)
	if err != nil {
		return err
	}
	if o.MirrorURL != "" {
		if err := checkURL(o.MirrorURL); err != nil {
			return fmt.Errorf("--mirror-url: %w", err)
		}
	}

	var undos []undo
	defer func() {
		if err != nil {
			err = errors.Join(err, s.rollback(undos))
		}
	}()

	// 1. Config: validate the key with the server before changing anything.
	cfg, u, err := s.installConfig(ctx, key, o.ServerURL)
	if err != nil {
		return err
	}
	undos = append(undos, u)

	// 2. Binary.
	bin, err := s.d.Binary.Install(o.SourceBinary)
	if err != nil {
		return fmt.Errorf("installing binary: %w", err)
	}
	undos = append(undos, undo{"binary", func() error { return s.d.Binary.Rollback(bin) }})
	if bin.Changed {
		s.say("binary", "installed %s", bin.Path)
	} else {
		s.say("binary", "%s is up to date", bin.Path)
	}

	// 3. .wakatime.cfg, then the mirror target in the config.
	u, err = s.installWaka(&cfg, o)
	if u.fn != nil {
		undos = append(undos, u)
	}
	if err != nil {
		return err
	}

	// 4. Harness hooks. A harness that fails is reported but doesn't undo
	// the install: editor tracking works without it.
	for _, h := range selected {
		u, ok := s.installHarness(h, s.d.Binary.Path())
		if ok && u.fn != nil {
			undos = append(undos, u)
		}
	}

	// 5. Autostart, then start the daemon now.
	before, err := s.d.Autostart.Installed()
	if err != nil {
		return fmt.Errorf("checking autostart: %w", err)
	}
	if !before {
		// Registered before Install: a partial entry (unit written but
		// not enabled, plist written but not loaded) must be removed too,
		// and Uninstall is safe on one.
		undos = append(undos, undo{"autostart", s.d.Autostart.Uninstall})
	}
	if err := s.d.Autostart.Install(s.d.Binary.Path()); err != nil {
		return fmt.Errorf("setting up autostart: %w", err)
	}
	s.say("autostart", "starts at login via %s", s.d.Autostart.Path())
	if err := s.d.Autostart.Start(); err != nil {
		return fmt.Errorf("starting the daemon: %w", err)
	}
	s.say("daemon", "started (local server http://%s)", cfg.LocalAddr)

	if err := s.d.Binary.Finish(bin); err != nil {
		s.say("binary", "could not delete the previous binary yet (%v); it is removed on the next install", err)
	}
	return nil
}

// installConfig loads or creates the config, checks the key with the
// server and saves the key and server. Its undo restores the previous
// config or deletes a new one.
func (s *Service) installConfig(ctx context.Context, key, serverURL string) (config.Config, undo, error) {
	prev, err := config.Load(s.d.DataDir)
	fresh := errors.Is(err, config.ErrNotConfigured)
	if err != nil && !fresh {
		return config.Config{}, undo{}, fmt.Errorf("reading %s: %w (fix or delete it, then retry)", config.Path(s.d.DataDir), err)
	}
	cfg := prev
	if fresh {
		if cfg, err = config.Default(); err != nil {
			return config.Config{}, undo{}, err
		}
	}
	if serverURL != "" {
		if err := checkURL(serverURL); err != nil {
			return config.Config{}, undo{}, fmt.Errorf("--server: %w", err)
		}
		cfg.ServerURL = strings.TrimRight(serverURL, "/")
	}
	if err := s.d.Keys.Check(ctx, cfg.ServerURL, key); err != nil {
		return config.Config{}, undo{}, err
	}
	cfg.APIKey = key
	if err := config.Save(s.d.DataDir, cfg); err != nil {
		return config.Config{}, undo{}, fmt.Errorf("saving config: %w", err)
	}
	s.say("config", "saved %s (server %s)", config.Path(s.d.DataDir), cfg.ServerURL)
	u := undo{"config", func() error { return config.Save(s.d.DataDir, prev) }}
	if fresh {
		u.fn = func() error { return os.Remove(config.Path(s.d.DataDir)) }
	}
	return cfg, u, nil
}

// installWaka redirects .wakatime.cfg and records the mirror target. A
// repeat install keeps the user's earlier mirror choice unless a mirror
// flag is given.
func (s *Service) installWaka(cfg *config.Config, o Options) (undo, error) {
	localURL := "http://" + cfg.LocalAddr + "/api/v1"
	res, err := s.d.Waka.Install(localURL, cfg.APIKey)
	if err != nil {
		return undo{}, fmt.Errorf("updating %s: %w", s.d.Waka.Path(), err)
	}
	// A first install is undone like uninstall; a repeat install puts back
	// the bytes and state it replaced (a new --key, say), so the file and
	// config.json never disagree after a rollback.
	u := undo{"wakatime config", func() error { return s.d.Waka.Revert(res) }}
	switch {
	case res.Changed && res.BackupWritten:
		s.say("wakatime", "pointed %s at Logbook (original backed up)", res.Path)
	case res.Changed:
		s.say("wakatime", "pointed %s at Logbook", res.Path)
	default:
		s.say("wakatime", "%s already points at Logbook", res.Path)
	}
	for _, w := range res.Warnings {
		s.say("wakatime", "note: %s", w)
	}

	keep := res.AlreadyInstalled && !o.NoMirror && o.MirrorURL == "" && cfg.Hackatime.URL != ""
	if !keep {
		cfg.Hackatime = wakacfg.MirrorTarget(res.Original, wakacfg.MirrorOptions{
			ServerURL: cfg.ServerURL, LocalURL: localURL,
			OverrideURL: o.MirrorURL, Disabled: o.NoMirror,
		})
	}
	if err := config.Save(s.d.DataDir, *cfg); err != nil {
		return u, fmt.Errorf("saving mirror settings: %w", err)
	}
	s.say("mirror", "%s", describeMirror(cfg.Hackatime))
	return u, nil
}

func describeMirror(h config.Hackatime) string {
	if !h.Mirror {
		if h.URL != "" && h.APIKey != "" {
			return "off (to turn it on, run install again with --mirror-url " + h.URL + ")"
		}
		return "off (no previous WakaTime or Hackatime key found)"
	}
	host := h.URL
	if p, err := url.Parse(h.URL); err == nil && p.Host != "" {
		host = p.Host
	}
	if wakacfg.IsHackatime(h.URL) {
		return "heartbeats are also sent to Hackatime (" + host + ")"
	}
	return "heartbeats are also sent to " + host + ", where they went before"
}

// installHarness installs one harness's hooks. ok is false when it was
// skipped or failed (and was reported).
func (s *Service) installHarness(h HarnessInstaller, binPath string) (undo, bool) {
	if !h.Detect() {
		s.say("hooks", "%s not found, skipped", h.Name())
		return undo{}, false
	}
	before, err := h.Installed()
	if err != nil {
		s.say("hooks", "%s: could not check existing hooks: %v", h.Name(), err)
		return undo{}, false
	}
	if err := h.Install(binPath); err != nil {
		s.say("hooks", "%s: FAILED: %v (the rest of the install continues)", h.Name(), err)
		return undo{}, false
	}
	if before {
		s.say("hooks", "%s hooks up to date", h.Name())
		return undo{}, true
	}
	s.say("hooks", "%s hooks installed", h.Name())
	return undo{"hooks " + h.Name(), h.Uninstall}, true
}

// selectHarnesses parses --harness. Unknown names are an error before
// anything changes.
func (s *Service) selectHarnesses(spec string) ([]HarnessInstaller, error) {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "", "all":
		return s.d.Harnesses, nil
	case "none":
		return nil, nil
	}
	byName := map[string]HarnessInstaller{}
	var names []string
	for _, h := range s.d.Harnesses {
		byName[h.Name()] = h
		names = append(names, h.Name())
	}
	var out []HarnessInstaller
	for _, n := range strings.Split(spec, ",") {
		n = strings.TrimSpace(n)
		h, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("unknown harness %q (known: all, none, %s)", n, strings.Join(names, ", "))
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (s *Service) rollback(undos []undo) error {
	var errs []error
	for i := len(undos) - 1; i >= 0; i-- {
		if err := undos[i].fn(); err != nil {
			s.say("undo", "%s: FAILED: %v", undos[i].name, err)
			errs = append(errs, fmt.Errorf("undoing %s: %w", undos[i].name, err))
			continue
		}
		s.say("undo", "%s reverted", undos[i].name)
	}
	return errors.Join(errs...)
}

// say prints one line of progress. Values printed here never include keys
// or tokens.
func (s *Service) say(step, format string, args ...any) {
	_, _ = fmt.Fprintf(s.d.Out, "  %-10s "+format+"\n", append([]any{step}, args...)...) // a failed progress write isn't worth failing the install for
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%q is not an http(s) URL", raw)
	}
	return nil
}
