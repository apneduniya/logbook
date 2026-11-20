// Package claudecode connects Claude Code to Logbook: it installs Logbook's
// command hooks into the user settings file (~/.claude/settings.json, or
// $CLAUDE_CONFIG_DIR/settings.json), points Claude Code's OpenTelemetry
// metrics exporter at the agent through that file's "env" object (unless the
// user or an administrator's managed settings configure telemetry, which it
// also re-checks for the daemon: YieldTelemetry), and maps Claude Code hook payloads to
// AI events. See docs/agent/harness.md for the event table, the telemetry
// entries and what is ignored.
package claudecode
