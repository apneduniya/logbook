package wire

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Harness identifies an AI coding tool.
type Harness string

// Supported harnesses in V1 (ADR 0009). New plugins add constants here.
const (
	HarnessClaudeCode Harness = "claude-code"
	HarnessCodex      Harness = "codex"
	HarnessGeminiCLI  Harness = "gemini-cli"
	HarnessOpenCode   Harness = "opencode"
	HarnessCursor     Harness = "cursor"
	HarnessCopilot    Harness = "copilot"
)

// AIEventKind is the normalised lifecycle step of an AI session.
type AIEventKind string

// Normalised AI event kinds. Each harness plugin maps its own hook names onto these.
const (
	AISessionStart  AIEventKind = "session_start"
	AISessionEnd    AIEventKind = "session_end"
	AIPromptSubmit  AIEventKind = "prompt_submit"  // the human sent a prompt
	AIToolStart     AIEventKind = "tool_start"     // the agent began a tool call
	AIToolEnd       AIEventKind = "tool_end"       // a tool call finished
	AIPermissionAsk AIEventKind = "permission_ask" // the agent is waiting for the human
	AITurnEnd       AIEventKind = "turn_end"       // the agent finished responding
)

// AIEvent is metadata about one AI harness event. It must never carry prompt
// text, code, file contents or tool output (ADR 0006, ADR 0009).
type AIEvent struct {
	Time      time.Time   `json:"time"`
	Harness   Harness     `json:"harness"`
	Kind      AIEventKind `json:"kind"`
	SessionID string      `json:"session_id"`
	// Project is the project name derived from the working directory.
	Project string `json:"project,omitempty"`
	// Cwd is kept locally only, to map events to projects and git repos.
	Cwd   string `json:"cwd,omitempty"`
	Model string `json:"model,omitempty"`
	// Tool is the tool name (e.g. "Edit", "Bash"), never its arguments.
	Tool string `json:"tool,omitempty"`
	// PromptChars is the prompt's length in characters, for prompt_submit only.
	PromptChars int `json:"prompt_chars,omitempty"`
	// Decision records the human's answer to a permission ask: "allow", "deny" or "".
	Decision string `json:"decision,omitempty"`
}

// ActivitySample is one sensor reading from the device (ADR 0007).
type ActivitySample struct {
	Time        time.Time `json:"time"`
	IdleSeconds float64   `json:"idle_seconds"`
	// FocusApp is the focused application's process name ("" if unknown).
	FocusApp string `json:"focus_app,omitempty"`
	// FocusKind classifies FocusApp: editor, terminal, browser, ai, other, unknown.
	FocusKind string `json:"focus_kind"`
}

// GitEvent is one observed change in a local repository.
type GitEvent struct {
	Time    time.Time `json:"time"`
	Project string    `json:"project"`
	// Kind is "commit" or "worktree" (uncommitted diff size snapshot).
	Kind         string `json:"kind"`
	CommitHash   string `json:"commit_hash,omitempty"`
	FilesChanged int    `json:"files_changed"`
	Insertions   int    `json:"insertions"`
	Deletions    int    `json:"deletions"`
}

const maxIDLen = 256

// Validate checks an AIEvent's required fields and bounds.
func (e AIEvent) Validate() error {
	switch {
	case e.Time.IsZero():
		return fmt.Errorf("%w: ai event time is required", ErrInvalidEvent)
	case e.Harness == "" || len(e.Harness) > 64:
		return fmt.Errorf("%w: harness is required", ErrInvalidEvent)
	case e.Kind == "" || len(e.Kind) > 64:
		return fmt.Errorf("%w: kind is required", ErrInvalidEvent)
	case len(e.SessionID) > maxIDLen || len(e.Project) > maxShortLen || len(e.Model) > maxShortLen ||
		len(e.Tool) > maxShortLen || len(e.Cwd) > maxEntityLen:
		return fmt.Errorf("%w: field too long", ErrInvalidEvent)
	case e.PromptChars < 0:
		return fmt.Errorf("%w: negative prompt length", ErrInvalidEvent)
	case e.Decision != "" && e.Decision != "allow" && e.Decision != "deny":
		return fmt.Errorf("%w: decision must be allow, deny or empty", ErrInvalidEvent)
	}
	return nil
}

// Validate checks an ActivitySample's bounds.
func (s ActivitySample) Validate() error {
	if s.Time.IsZero() || s.IdleSeconds < 0 || len(s.FocusApp) > maxShortLen {
		return fmt.Errorf("%w: bad activity sample", ErrInvalidEvent)
	}
	switch s.FocusKind {
	case "editor", "terminal", "browser", "ai", "other", "unknown":
		return nil
	}
	return fmt.Errorf("%w: unknown focus kind %q", ErrInvalidEvent, s.FocusKind)
}

// Validate checks a GitEvent's bounds.
func (g GitEvent) Validate() error {
	if g.Time.IsZero() || strings.TrimSpace(g.Project) == "" || len(g.Project) > maxShortLen ||
		(g.Kind != "commit" && g.Kind != "worktree") || len(g.CommitHash) > 64 ||
		g.FilesChanged < 0 || g.Insertions < 0 || g.Deletions < 0 {
		return fmt.Errorf("%w: bad git event", ErrInvalidEvent)
	}
	return nil
}

// ErrInvalidEvent is wrapped by every event validation failure.
var ErrInvalidEvent = errors.New("invalid event")
