package claudecode

import (
	"encoding/json"
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

// NewInstaller returns the Claude Code installer.
func NewInstaller(env hostenv.Env) *hookfile.Installer {
	return hookfile.New(hookfile.Spec{
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
	})
}

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
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	Model         string          `json:"model"`
	ToolName      string          `json:"tool_name"`
	Prompt        aievent.TextLen `json:"prompt"`
	CursorVersion string          `json:"cursor_version"`
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
	if p.CursorVersion != "" || getenv("CURSOR_VERSION") != "" {
		return nil
	}
	f := aievent.Fields{Kind: kind, SessionID: p.SessionID, Cwd: p.Cwd, Model: p.Model}
	switch kind {
	case wire.AIToolStart, wire.AIToolEnd, wire.AIPermissionAsk:
		f.Tool = p.ToolName
	case wire.AIPromptSubmit:
		f.PromptChars = int(p.Prompt)
	}
	return []aievent.Fields{f}
}

// Response is what the hook prints on stdout. Claude Code treats exit 0 with
// no output as "carry on" for every event.
func Response(string) []byte { return nil }
