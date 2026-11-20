// Package cursor connects Cursor (the editor's Agent and the Cursor CLI) to
// Logbook. It installs command hooks into the user hooks file
// ~/.cursor/hooks.json and maps Cursor hook payloads to AI events.
//
// Cursor treats a bad response from a "permission" hook (preToolUse,
// beforeShellExecution, beforeMCPExecution, beforeReadFile, subagentStart)
// as a deny, and an "allow" could skip the user's own approval prompts, so
// Logbook installs none of them. Tool start times come instead from the
// duration Cursor reports in postToolUse. See docs/agent/harness.md.
package cursor
