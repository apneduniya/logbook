package claudecode

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/cmdline"
	"github.com/apneduniya/logbook/internal/agent/harness/hookfile"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Name is the harness id in hook command lines.
const Name = string(wire.HarnessClaudeCode)

// hookTimeout is the per-hook timeout written into settings, in seconds. The
// hook itself finishes in well under a second.
const hookTimeout = 10

// Events maps the Claude Code events Logbook installs, in install order, to
// AI event kinds. SubagentStart/SubagentStop are left out on purpose: a
// subagent runs inside the Agent tool call, which PreToolUse/PostToolUse
// already bracket. Notification is left out because its permission_prompt
// type fires only after ~6 s without typing; PermissionRequest fires the
// moment Claude asks.
func Events() aievent.Table {
	return aievent.Table{
		{Event: "SessionStart", Kind: wire.AISessionStart},
		{Event: "UserPromptSubmit", Kind: wire.AIPromptSubmit},
		{Event: "PreToolUse", Kind: wire.AIToolStart},
		{Event: "PostToolUse", Kind: wire.AIToolEnd},
		{Event: "PostToolUseFailure", Kind: wire.AIToolEnd},
		{Event: "PermissionRequest", Kind: wire.AIPermissionAsk},
		{Event: "Stop", Kind: wire.AITurnEnd},
		{Event: "StopFailure", Kind: wire.AITurnEnd},
		{Event: "SessionEnd", Kind: wire.AISessionEnd},
	}
}

// SettingsPath returns the user settings file Logbook edits.
func SettingsPath(env hostenv.Env) (string, error) {
	dir, err := env.DirFromEnv("CLAUDE_CONFIG_DIR", ".claude")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// Installer installs Logbook's hooks into Claude Code's settings.json and
// points its OpenTelemetry metrics exporter at the agent (telemetry.go).
type Installer struct {
	*hookfile.Installer
	env hostenv.Env
}

// NewInstaller returns the Claude Code installer.
func NewInstaller(env hostenv.Env) *Installer {
	return &Installer{env: env, Installer: hookfile.New(hookfile.Spec{
		Harness: Name,
		Path:    func() (string, error) { return SettingsPath(env) },
		Layout:  hookfile.Nested,
		Entries: entries,
		Argv:    hookfile.CommandArgv,
		Detect: func() bool {
			dir, err := env.DirFromEnv("CLAUDE_CONFIG_DIR", ".claude")
			return (err == nil && hostenv.Exists(dir)) || env.OnPath("claude")
		},
		BackupDir: env.BackupDir,
	})}
}

// Install adds the hooks, then the telemetry env entries unless the user
// has their own OpenTelemetry setup.
func (in *Installer) Install(binPath string) error {
	if err := in.Installer.Install(binPath); err != nil {
		return err
	}
	if err := installTelemetry(in.env); err != nil {
		return fmt.Errorf("%s: telemetry: %w", Name, err)
	}
	return nil
}

// Uninstall removes the telemetry entries Logbook added, then the hooks
// (which deletes settings.json if Logbook created it and nothing is left).
func (in *Installer) Uninstall() error {
	if err := uninstallTelemetry(in.env); err != nil {
		return fmt.Errorf("%s: telemetry: %w", Name, err)
	}
	return in.Installer.Uninstall()
}

// Notes says when telemetry was left alone, and why.
func (in *Installer) Notes() []string { return telemetryNotes(in.env) }

// handler is a command hook in exec form: Claude Code (v2.1.139+) spawns
// the binary directly with args, so no shell quoting is involved on any OS.
type handler struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

func entries(binPath string) []hookfile.Entry {
	evs := Events()
	out := make([]hookfile.Entry, 0, len(evs))
	for _, e := range evs {
		ev := e.Event
		h, _ := json.Marshal(handler{ // marshalling plain strings can't fail
			Type: "command", Command: binPath, Args: cmdline.HookArgs(Name, ev), Timeout: hookTimeout,
		})
		out = append(out, hookfile.Entry{Event: ev, Handler: h})
	}
	return out
}

// payload holds the fields Logbook reads. Everything else, including
// prompt, tool_input, tool_response, last_assistant_message, transcript_path
// and session_title, is never decoded into a field.
type payload struct {
	SessionID     string         `json:"session_id"`
	Cwd           string         `json:"cwd"`
	Model         string         `json:"model"`
	ToolName      string         `json:"tool_name"`
	Prompt        aievent.Prompt `json:"prompt"`
	CursorVersion string         `json:"cursor_version"`
}

// Parse maps a hook payload to AI event fields. It returns nil for events
// Logbook doesn't record, and for Claude-format hooks that Cursor runs (it
// loads ~/.claude/settings.json too); Logbook's own Cursor hooks record those.
func Parse(event string, in aievent.Input, getenv func(string) string) []aievent.Fields {
	kind, ok := Events().Kind(event)
	if !ok {
		return nil
	}
	var p payload
	if aievent.Decode(in, &p) != nil {
		return nil
	}
	if runByCursor(p, getenv) {
		return nil
	}
	f := aievent.Fields{Kind: kind, SessionID: p.SessionID, Cwd: p.Cwd, Model: p.Model}
	switch kind {
	case wire.AIToolStart, wire.AIToolEnd, wire.AIPermissionAsk:
		f.Tool = p.ToolName
	case wire.AIPromptSubmit:
		f.PromptChars, f.PromptDigest = p.Prompt.Len(), p.Prompt.Digest()
	}
	return []aievent.Fields{f}
}

// runByCursor reports whether Cursor, not Claude Code, ran this hook. Cursor
// documents cursor_version in its hook payloads and CURSOR_VERSION in hook
// environments, but not whether its Claude-format payloads keep the field,
// so the env var is checked too. Claude Code (2.1.172+) sets
// CLAUDE_CODE_CHILD_SESSION=1 on hooks it runs itself, which keeps a real
// Claude session recorded when Cursor's environment leaked into its shell.
func runByCursor(p payload, getenv func(string) string) bool {
	if p.CursorVersion != "" {
		return true
	}
	return getenv("CURSOR_VERSION") != "" && getenv("CLAUDE_CODE_CHILD_SESSION") != "1"
}

// Response is what the hook prints on stdout. Claude Code treats exit 0 with
// no output as "carry on" for every event.
func Response(string) []byte { return nil }
