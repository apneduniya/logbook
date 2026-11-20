// Package copilot connects GitHub Copilot to Logbook through the Copilot
// hooks format, which Copilot CLI, VS Code's Copilot agent harness and the
// Copilot app share. Logbook owns one user-level hook file,
// ~/.copilot/hooks/logbook.json ($COPILOT_HOME/hooks/logbook.json), and
// writes exec-form handlers, so no shell is involved.
//
// Copilot fails closed when a preToolUse command hook errors (a missing
// binary would block every tool call), so Logbook doesn't install preToolUse;
// tool calls are recorded when they finish. Copilot inline completions and
// VS Code's deprecated "Local" chat harness don't run these hooks. See
// docs/agent/harness.md.
package copilot
