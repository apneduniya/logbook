package geminicli

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
const Name = string(wire.HarnessGeminiCLI)

// hookTimeoutMS is the per-hook timeout; Gemini CLI uses milliseconds.
const hookTimeoutMS = 5000

// Events maps the Gemini CLI events Logbook installs, in install order, to
// AI event kinds. BeforeModel, AfterModel and BeforeToolSelection are left
// out: they carry the whole LLM request and fire per streamed chunk.
// Notification is recorded only for its ToolPermission type (see Parse).
func Events() aievent.Table {
	return aievent.Table{
		{Event: "SessionStart", Kind: wire.AISessionStart},
		{Event: "BeforeAgent", Kind: wire.AIPromptSubmit},
		{Event: "BeforeTool", Kind: wire.AIToolStart},
		{Event: "AfterTool", Kind: wire.AIToolEnd},
		{Event: "Notification", Kind: wire.AIPermissionAsk},
		{Event: "AfterAgent", Kind: wire.AITurnEnd},
		{Event: "SessionEnd", Kind: wire.AISessionEnd},
	}
}

// configDir returns Gemini CLI's global dir. Gemini CLI uses
// $GEMINI_CLI_HOME in place of the home directory when it is set
// (homedir() in packages/core/src/utils/paths.ts).
func configDir(env hostenv.Env) (string, error) {
	home, err := env.DirFromEnv("GEMINI_CLI_HOME")
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini"), nil
}

// SettingsPath returns the user settings file.
func SettingsPath(env hostenv.Env) (string, error) {
	dir, err := configDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// NewInstaller returns the Gemini CLI installer.
func NewInstaller(env hostenv.Env) *hookfile.Installer {
	sh := cmdline.POSIX // `bash -c` on macOS and Linux
	if env.GOOS == "windows" {
		sh = cmdline.PowerShell // pwsh or powershell.exe -Command on Windows
	}
	return hookfile.New(hookfile.Spec{
		Harness: Name,
		Path:    func() (string, error) { return SettingsPath(env) },
		Layout:  hookfile.Nested,
		// Gemini CLI reads settings.json with comments allowed. Logbook
		// edits only the "hooks" member in place, so the user's comments
		// elsewhere are kept.
		JSONC:   true,
		Entries: func(binPath string) []hookfile.Entry { return entries(sh, binPath) },
		Argv:    hookfile.CommandArgv,
		Detect: func() bool {
			dir, err := configDir(env)
			return (err == nil && hostenv.Exists(dir)) || env.OnPath("gemini")
		},
		BackupDir: env.BackupDir,
	})
}

type handler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Name    string `json:"name"`
	Timeout int    `json:"timeout"`
}

func entries(sh cmdline.Shell, binPath string) []hookfile.Entry {
	evs := Events()
	out := make([]hookfile.Entry, 0, len(evs))
	for _, e := range evs {
		h, _ := json.Marshal(handler{ // marshalling plain strings can't fail
			Type: "command", Command: cmdline.Quote(sh, cmdline.Hook(binPath, Name, e.Event)),
			Name: "logbook", Timeout: hookTimeoutMS,
		})
		out = append(out, hookfile.Entry{Event: e.Event, Handler: h})
	}
	return out
}

// payload holds the fields Logbook reads. prompt is decoded only as a
// length; tool_input, tool_response, prompt_response, message, details and
// transcript_path are never decoded.
type payload struct {
	SessionID        string          `json:"session_id"`
	Cwd              string          `json:"cwd"`
	ToolName         string          `json:"tool_name"`
	Prompt           aievent.TextLen `json:"prompt"`
	NotificationType string          `json:"notification_type"`
}

// Parse maps a hook payload to AI event fields, or nil if Logbook doesn't
// record the event. Gemini CLI hook payloads carry no model name.
func Parse(event string, in aievent.Input, _ func(string) string) []aievent.Fields {
	kind, ok := Events().Kind(event)
	if !ok {
		return nil
	}
	var p payload
	if aievent.Decode(in, &p) != nil {
		return nil
	}
	if event == "Notification" && p.NotificationType != "ToolPermission" {
		return nil
	}
	f := aievent.Fields{Kind: kind, SessionID: p.SessionID, Cwd: p.Cwd}
	switch kind {
	case wire.AIToolStart, wire.AIToolEnd:
		f.Tool = p.ToolName
	case wire.AIPromptSubmit:
		f.PromptChars = int(p.Prompt)
	}
	return []aievent.Fields{f}
}

// Response is what the hook prints on stdout. Gemini CLI parses stdout as
// JSON and treats empty output as "no decision"; it would parse stderr if
// stdout were empty, so the hook writes nothing to either.
func Response(string) []byte { return nil }
