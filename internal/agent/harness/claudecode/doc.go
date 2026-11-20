// Package claudecode connects Claude Code to Logbook: it installs Logbook's
// command hooks into the user settings file (~/.claude/settings.json, or
// $CLAUDE_CONFIG_DIR/settings.json) and maps Claude Code hook payloads to AI
// events. See docs/agent/harness.md for the event table and what is ignored.
package claudecode
