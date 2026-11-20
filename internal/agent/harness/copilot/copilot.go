package copilot

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
const Name = string(wire.HarnessCopilot)

// hookTimeout is the per-hook timeout in seconds.
const hookTimeout = 5

// Events maps the Copilot events Logbook installs, in install order, to AI
// event kinds. Event names are camelCase, which selects Copilot's native
// payload (camelCase fields). notification is recorded only for its
// permission_prompt type (see Parse).
func Events() aievent.Table {
	return aievent.Table{
		{Event: "sessionStart", Kind: wire.AISessionStart},
		{Event: "userPromptSubmitted", Kind: wire.AIPromptSubmit},
		{Event: "postToolUse", Kind: wire.AIToolEnd},
		{Event: "postToolUseFailure", Kind: wire.AIToolEnd},
		{Event: "notification", Kind: wire.AIPermissionAsk},
		{Event: "agentStop", Kind: wire.AITurnEnd},
		{Event: "sessionEnd", Kind: wire.AISessionEnd},
	}
}

func homeDir(env hostenv.Env) (string, error) {
	return env.DirFromEnv("COPILOT_HOME", ".copilot")
}

// HooksPath returns the hook file Logbook owns.
func HooksPath(env hostenv.Env) (string, error) {
	dir, err := homeDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hooks", "logbook.json"), nil
}

// NewInstaller returns the Copilot installer.
func NewInstaller(env hostenv.Env) *hookfile.Installer {
	return hookfile.New(hookfile.Spec{
		Harness: Name,
		Path:    func() (string, error) { return HooksPath(env) },
		Layout:  hookfile.Flat,
		Header:  []hookfile.Field{{Key: "version", Value: json.RawMessage("1")}},
		Entries: entries,
		Argv:    hookfile.CopilotArgv,
		Detect: func() bool {
			dir, err := homeDir(env)
			return (err == nil && hostenv.Exists(dir)) || env.OnPath("copilot")
		},
		BackupDir: env.BackupDir,
	})
}

type handler struct {
	Type       string   `json:"type"`
	Exec       string   `json:"exec"`
	Args       []string `json:"args"`
	TimeoutSec int      `json:"timeoutSec"`
}

func entries(binPath string) []hookfile.Entry {
	evs := Events()
	out := make([]hookfile.Entry, 0, len(evs))
	for _, e := range evs {
		h, _ := json.Marshal(handler{ // marshalling plain strings can't fail
			Type: "command", Exec: binPath, Args: cmdline.HookArgs(Name, e.Event), TimeoutSec: hookTimeout,
		})
		out = append(out, hookfile.Entry{Event: e.Event, Handler: h})
	}
	return out
}

// payload holds the fields Logbook reads. prompt is decoded only as a
// length; initialPrompt, toolArgs, toolResult, message, title and
// transcriptPath are never decoded. Copilot payloads carry no model name.
type payload struct {
	SessionID        string         `json:"sessionId"`
	Cwd              string         `json:"cwd"`
	ToolName         string         `json:"toolName"`
	Prompt           aievent.Prompt `json:"prompt"`
	NotificationType string         `json:"notification_type"`
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
	if event == "notification" && p.NotificationType != "permission_prompt" {
		return nil
	}
	f := aievent.Fields{Kind: kind, SessionID: p.SessionID, Cwd: p.Cwd}
	switch kind {
	case wire.AIToolEnd:
		f.Tool = p.ToolName
	case wire.AIPromptSubmit:
		f.PromptChars, f.PromptDigest = p.Prompt.Len(), p.Prompt.Digest()
	}
	return []aievent.Fields{f}
}

// Response is what the hook prints on stdout. Copilot accepts empty output
// as "no action" for every event.
func Response(string) []byte { return nil }
