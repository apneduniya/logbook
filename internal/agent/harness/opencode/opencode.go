package opencode

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/hookfile"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Name is the harness id in hook command lines.
const Name = string(wire.HarnessOpenCode)

// Events maps the plugin's event names to AI event kinds. permission-reply
// is a permission_ask carrying the human's answer in Decision.
func Events() aievent.Table {
	return aievent.Table{
		{Event: "session-start", Kind: wire.AISessionStart},
		{Event: "prompt", Kind: wire.AIPromptSubmit},
		{Event: "tool-start", Kind: wire.AIToolStart},
		{Event: "tool-end", Kind: wire.AIToolEnd},
		{Event: "permission-ask", Kind: wire.AIPermissionAsk},
		{Event: "permission-reply", Kind: wire.AIPermissionAsk},
		{Event: "turn-end", Kind: wire.AITurnEnd},
		{Event: "session-end", Kind: wire.AISessionEnd},
	}
}

// ConfigDir returns OpenCode's global config directory. OpenCode follows
// XDG on every OS, so on Windows too it is %USERPROFILE%\.config\opencode.
func ConfigDir(env hostenv.Env) (string, error) {
	if x := env.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "opencode"), nil
	}
	return env.HomePath(".config", "opencode")
}

// PluginPath returns the plugin file Logbook owns.
func PluginPath(env hostenv.Env) (string, error) {
	dir, err := ConfigDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plugins", "logbook.js"), nil
}

// ErrNotOurs means a file Logbook didn't write sits where its plugin goes.
var ErrNotOurs = errors.New("file exists and was not written by Logbook; leaving it alone")

// Installer writes and removes the OpenCode plugin.
type Installer struct {
	env hostenv.Env
}

// NewInstaller returns the OpenCode installer.
func NewInstaller(env hostenv.Env) *Installer { return &Installer{env: env} }

// Name returns the harness id.
func (in *Installer) Name() string { return Name }

// Detect reports whether OpenCode looks installed.
func (in *Installer) Detect() bool {
	dir, err := ConfigDir(in.env)
	return (err == nil && hostenv.Exists(dir)) || in.env.OnPath("opencode")
}

// ConfigPath returns the plugin file path.
func (in *Installer) ConfigPath() (string, error) { return PluginPath(in.env) }

// Install writes the plugin for binPath. It is a no-op when the file already
// has that content, and refuses to replace a file Logbook didn't write.
func (in *Installer) Install(binPath string) error {
	if !filepath.IsAbs(binPath) {
		return fmt.Errorf("%s: %w: %q", Name, hookfile.ErrRelativeBin, binPath)
	}
	path, cur, existed, err := in.read()
	if err != nil {
		return err
	}
	want := renderPlugin(binPath)
	if existed {
		if bytes.Equal(cur, want) {
			return nil
		}
		if !bytes.Contains(cur, []byte(marker)) {
			return fmt.Errorf("%s: %s: %w", Name, path, ErrNotOurs)
		}
	}
	if err := hookfile.WriteAtomic(path, want, 0o644); err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	return nil
}

// Uninstall removes the plugin if Logbook wrote it.
func (in *Installer) Uninstall() error {
	path, cur, existed, err := in.read()
	if err != nil || !existed || !bytes.Contains(cur, []byte(marker)) {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: removing %s: %w", Name, path, err)
	}
	return nil
}

// Installed reports whether Logbook's plugin is in place.
func (in *Installer) Installed() (bool, error) {
	_, cur, existed, err := in.read()
	if err != nil {
		return false, err
	}
	return existed && bytes.Contains(cur, []byte(marker)), nil
}

func (in *Installer) read() (path string, data []byte, existed bool, err error) {
	path, err = PluginPath(in.env)
	if err != nil {
		return "", nil, false, fmt.Errorf("%s: finding config: %w", Name, err)
	}
	data, existed, _, err = hookfile.ReadFile(path)
	if err != nil {
		return "", nil, false, fmt.Errorf("%s: %w", Name, err)
	}
	return path, data, existed, nil
}

// payload is the metadata the plugin sends. It never contains prompt text,
// tool arguments or output; the plugin computes prompt_chars itself.
type payload struct {
	SessionID   string `json:"session_id"`
	Cwd         string `json:"cwd"`
	Model       string `json:"model"`
	Tool        string `json:"tool"`
	PromptChars int    `json:"prompt_chars"`
	Decision    string `json:"decision"`
}

// Parse maps the plugin's payload to AI event fields, or nil for an unknown
// event.
func Parse(event string, in aievent.Input, _ func(string) string) []aievent.Fields {
	kind, ok := Events().Kind(event)
	if !ok {
		return nil
	}
	var p payload
	if aievent.Decode(in, &p) != nil {
		return nil
	}
	f := aievent.Fields{Kind: kind, SessionID: p.SessionID, Cwd: p.Cwd, Model: p.Model}
	switch kind {
	case wire.AIToolStart, wire.AIToolEnd:
		f.Tool = p.Tool
	case wire.AIPromptSubmit:
		if p.PromptChars > 0 {
			f.PromptChars = p.PromptChars
		}
	case wire.AIPermissionAsk:
		if event == "permission-reply" {
			f.Decision = p.Decision
		}
	}
	return []aievent.Fields{f}
}

// Response is what the hook prints on stdout. The plugin ignores it.
func Response(string) []byte { return nil }
