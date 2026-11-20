package cursor

import (
	"encoding/json"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/cmdline"
	"github.com/apneduniya/logbook/internal/agent/harness/hookfile"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Name is the harness id in hook command lines.
const Name = string(wire.HarnessCursor)

// hookTimeout is the per-hook timeout in seconds.
const hookTimeout = 5

// Events maps the Cursor events Logbook installs, in install order, to AI
// event kinds. Cursor has no hook for "waiting for the human to approve", so
// it never produces permission_ask, and none Logbook can safely install for a
// tool's start, so it never produces tool_start (see Parse).
func Events() aievent.Table {
	return aievent.Table{
		{Event: "sessionStart", Kind: wire.AISessionStart},
		{Event: "beforeSubmitPrompt", Kind: wire.AIPromptSubmit},
		{Event: "postToolUse", Kind: wire.AIToolEnd},
		{Event: "postToolUseFailure", Kind: wire.AIToolEnd},
		{Event: "stop", Kind: wire.AITurnEnd},
		{Event: "sessionEnd", Kind: wire.AISessionEnd},
	}
}

// HooksPath returns the user hooks file.
func HooksPath(env hostenv.Env) (string, error) {
	return env.HomePath(".cursor", "hooks.json")
}

// NewInstaller returns the Cursor installer.
func NewInstaller(env hostenv.Env) *hookfile.Installer {
	sh := cmdline.POSIX
	if env.GOOS == "windows" {
		sh = cmdline.PowerShell // the desktop app runs hook commands through PowerShell
	}
	return hookfile.New(hookfile.Spec{
		Harness: Name,
		Path:    func() (string, error) { return HooksPath(env) },
		Layout:  hookfile.Flat,
		Header:  []hookfile.Field{{Key: "version", Value: json.RawMessage("1")}},
		Entries: func(binPath string) []hookfile.Entry { return entries(sh, binPath) },
		Argv:    hookfile.CommandArgv,
		Detect: func() bool {
			dir, err := env.HomePath(".cursor")
			return (err == nil && hostenv.Exists(dir)) || env.OnPath("cursor", "cursor-agent")
		},
		BackupDir: env.BackupDir,
	})
}

type handler struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func entries(sh cmdline.Shell, binPath string) []hookfile.Entry {
	evs := Events()
	out := make([]hookfile.Entry, 0, len(evs))
	for _, e := range evs {
		h, _ := json.Marshal(handler{ // marshalling plain strings can't fail
			Command: cmdline.Quote(sh, cmdline.Hook(binPath, Name, e.Event)), Timeout: hookTimeout,
		})
		out = append(out, hookfile.Entry{Event: e.Event, Handler: h})
	}
	return out
}

// payload holds the fields Logbook reads. prompt is decoded only as a
// length; attachments, tool_input, tool_output, text, user_email and
// transcript_path are never decoded. User hooks run with ~/.cursor as their
// working directory, so the project comes from workspace_roots (or the
// tool's cwd), never from the hook's own directory.
type payload struct {
	ConversationID string         `json:"conversation_id"`
	SessionID      string         `json:"session_id"`
	Model          string         `json:"model"`
	ModelID        string         `json:"model_id"`
	WorkspaceRoots []string       `json:"workspace_roots"`
	Cwd            string         `json:"cwd"`
	ToolName       string         `json:"tool_name"`
	Prompt         aievent.Prompt `json:"prompt"`
}

// Parse maps a hook payload to AI event fields, or nil if Logbook doesn't
// record the event. Every event is stamped when its hook runs. A finished
// tool call is one tool_end: a tool_start back-dated by the call's duration
// would reach the agent only when the call ends, possibly after the minutes
// it would change were final (docs/STRUGGLE-RULES.md).
func Parse(event string, in aievent.Input, _ func(string) string) []aievent.Fields {
	kind, ok := Events().Kind(event)
	if !ok {
		return nil
	}
	var p payload
	if aievent.Decode(in, &p) != nil {
		return nil
	}
	f := aievent.Fields{Kind: kind, SessionID: p.ConversationID, Model: p.ModelID}
	if f.SessionID == "" {
		f.SessionID = p.SessionID
	}
	if f.Model == "" {
		f.Model = p.Model
	}
	switch {
	case p.Cwd != "":
		f.Cwd = p.Cwd
	case len(p.WorkspaceRoots) > 0:
		f.Cwd = p.WorkspaceRoots[0]
	}
	switch kind {
	case wire.AIPromptSubmit:
		f.PromptChars, f.PromptDigest = p.Prompt.Len(), p.Prompt.Digest()
	case wire.AIToolEnd:
		f.Tool = p.ToolName
	}
	return []aievent.Fields{f}
}

// Response is what the hook prints on stdout. beforeSubmitPrompt reads a
// "continue" decision, so it gets an explicit continue; the other installed
// hooks accept an empty JSON object.
func Response(event string) []byte {
	if event == "beforeSubmitPrompt" {
		return []byte(`{"continue":true}` + "\n")
	}
	return []byte("{}\n")
}
