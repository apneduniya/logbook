package codex

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
const Name = string(wire.HarnessCodex)

// Timeouts written into hooks.json, in seconds. SessionEnd and Interrupt
// accept at most 3.
const (
	hookTimeout  = 10
	shortTimeout = 3
)

// Events maps the Codex events Logbook installs, in install order, to AI
// event kinds. Interrupt (the user stopped a turn) ends the turn.
// PreModelRequest/PostModelRequest and the compaction and subagent events are
// left out: they add volume without telling us more about who is working.
func Events() aievent.Table {
	return aievent.Table{
		{Event: "SessionStart", Kind: wire.AISessionStart},
		{Event: "UserPromptSubmit", Kind: wire.AIPromptSubmit},
		{Event: "PreToolUse", Kind: wire.AIToolStart},
		{Event: "PostToolUse", Kind: wire.AIToolEnd},
		{Event: "PermissionRequest", Kind: wire.AIPermissionAsk},
		{Event: "Stop", Kind: wire.AITurnEnd},
		{Event: "Interrupt", Kind: wire.AITurnEnd},
		{Event: "SessionEnd", Kind: wire.AISessionEnd},
	}
}

// HooksPath returns the user hooks file.
func HooksPath(env hostenv.Env) (string, error) {
	dir, err := env.DirFromEnv("CODEX_HOME", ".codex")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hooks.json"), nil
}

// NewInstaller returns the Codex installer.
func NewInstaller(env hostenv.Env) *hookfile.Installer {
	sh := cmdline.POSIX // Codex runs hooks with `$SHELL -lc`
	if env.GOOS == "windows" {
		sh = cmdline.Cmd // and with `%COMSPEC% /C` on Windows
	}
	return hookfile.New(hookfile.Spec{
		Harness: Name,
		Path:    func() (string, error) { return HooksPath(env) },
		Layout:  hookfile.Nested,
		Entries: func(binPath string) []hookfile.Entry { return entries(sh, binPath) },
		Argv:    hookfile.CommandArgv,
		Detect: func() bool {
			dir, err := env.DirFromEnv("CODEX_HOME", ".codex")
			return (err == nil && hostenv.Exists(dir)) || env.OnPath("codex")
		},
		BackupDir: env.BackupDir,
	})
}

type handler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func entries(sh cmdline.Shell, binPath string) []hookfile.Entry {
	evs := Events()
	out := make([]hookfile.Entry, 0, len(evs))
	for _, e := range evs {
		timeout := hookTimeout
		if e.Event == "SessionEnd" || e.Event == "Interrupt" {
			timeout = shortTimeout
		}
		h, _ := json.Marshal(handler{ // marshalling plain strings can't fail
			Type: "command", Command: cmdline.Quote(sh, cmdline.Hook(binPath, Name, e.Event)), Timeout: timeout,
		})
		out = append(out, hookfile.Entry{Event: e.Event, Handler: h})
	}
	return out
}

// payload holds the fields Logbook reads. prompt is decoded only as a
// length; tool_input, tool_response, last_assistant_message and
// transcript_path are never decoded.
type payload struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	Model     string          `json:"model"`
	ToolName  string          `json:"tool_name"`
	Prompt    aievent.TextLen `json:"prompt"`
}

// Parse maps a hook payload to AI event fields, or nil if Logbook doesn't
// record the event.
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
	case wire.AIToolStart, wire.AIToolEnd, wire.AIPermissionAsk:
		f.Tool = p.ToolName
	case wire.AIPromptSubmit:
		f.PromptChars = int(p.Prompt)
	}
	return []aievent.Fields{f}
}

// Response is what the hook prints on stdout. Codex treats exit 0 with empty
// stdout as success for every event (Stop and SubagentStop reject plain text
// but skip empty output).
func Response(string) []byte { return nil }
