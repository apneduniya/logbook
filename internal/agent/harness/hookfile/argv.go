package hookfile

import (
	"encoding/json"

	"github.com/apneduniya/logbook/internal/agent/harness/cmdline"
)

// handlerCommand is the subset of handler fields that say what runs. Each
// harness uses some of them.
type handlerCommand struct {
	Type       string   `json:"type"`
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	Exec       string   `json:"exec"`
	Bash       string   `json:"bash"`
	PowerShell string   `json:"powershell"`
}

// CommandArgv reads a handler whose "command" is a shell string, or, when
// "args" is present, an executable run with those args (Claude Code's exec
// form). Handlers whose "type" is set to something other than "command" run
// no command.
func CommandArgv(h json.RawMessage) []string {
	var c handlerCommand
	if json.Unmarshal(h, &c) != nil || (c.Type != "" && c.Type != "command") {
		return nil
	}
	if c.Args != nil {
		return append([]string{c.Command}, c.Args...)
	}
	return cmdline.Split(c.Command)
}

// CopilotArgv reads a GitHub Copilot handler: "exec" + "args", or a shell
// string in "bash", "powershell" or "command". Each candidate is tried so a
// Logbook entry is found whichever form wrote it.
func CopilotArgv(h json.RawMessage) []string {
	var c handlerCommand
	if json.Unmarshal(h, &c) != nil || (c.Type != "" && c.Type != "command") {
		return nil
	}
	if c.Exec != "" {
		return append([]string{c.Exec}, c.Args...)
	}
	for _, s := range []string{c.Bash, c.PowerShell, c.Command} {
		if s != "" {
			return cmdline.Split(s)
		}
	}
	return nil
}
