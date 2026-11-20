package harness

import (
	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/claudecode"
	"github.com/apneduniya/logbook/internal/agent/harness/codex"
	"github.com/apneduniya/logbook/internal/agent/harness/copilot"
	"github.com/apneduniya/logbook/internal/agent/harness/cursor"
	"github.com/apneduniya/logbook/internal/agent/harness/geminicli"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/internal/agent/harness/opencode"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Installer installs Logbook's hooks into one harness. It matches
// install.HarnessInstaller structurally; this package doesn't import install.
type Installer interface {
	Name() string
	Detect() bool
	Install(binPath string) error
	Uninstall() error
	Installed() (bool, error)
}

// Noter is implemented by installers that may have something to tell the
// user after installing (Codex: inline hooks in config.toml make it warn at
// startup). The install command can check for it with a type assertion and
// print each note; an empty result means nothing to say.
type Noter interface {
	Notes() []string
}

// All returns an installer for every supported harness on this machine, in
// a stable order.
func All() []Installer { return AllFor(hostenv.FromOS()) }

// AllFor returns the installers for env, in the same order as All.
func AllFor(env hostenv.Env) []Installer {
	return []Installer{
		claudecode.NewInstaller(env),
		codex.NewInstaller(env),
		geminicli.NewInstaller(env),
		opencode.NewInstaller(env),
		cursor.NewInstaller(env),
		copilot.NewInstaller(env),
	}
}

// Hook is how one harness's hook payloads are read.
type Hook struct {
	Harness  wire.Harness
	parse    func(event string, in aievent.Input, getenv func(string) string) []aievent.Fields
	response func(event string) []byte
}

// Parse extracts metadata from a payload; nil means "don't record".
func (h Hook) Parse(event string, in aievent.Input, getenv func(string) string) []aievent.Fields {
	return h.parse(event, in, getenv)
}

// Response is what the hook must print on stdout for event.
func (h Hook) Response(event string) []byte { return h.response(event) }

// Lookup returns the hook reader for a harness id.
func Lookup(name string) (Hook, bool) {
	switch name {
	case claudecode.Name:
		return Hook{wire.HarnessClaudeCode, claudecode.Parse, claudecode.Response}, true
	case codex.Name:
		return Hook{wire.HarnessCodex, codex.Parse, codex.Response}, true
	case geminicli.Name:
		return Hook{wire.HarnessGeminiCLI, geminicli.Parse, geminicli.Response}, true
	case opencode.Name:
		return Hook{wire.HarnessOpenCode, opencode.Parse, opencode.Response}, true
	case cursor.Name:
		return Hook{wire.HarnessCursor, cursor.Parse, cursor.Response}, true
	case copilot.Name:
		return Hook{wire.HarnessCopilot, copilot.Parse, copilot.Response}, true
	}
	return Hook{}, false
}
