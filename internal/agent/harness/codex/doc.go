// Package codex connects the OpenAI Codex CLI (and the Codex IDE extension
// and desktop app, which share its config) to Logbook. It installs command
// hooks into the user hooks file $CODEX_HOME/hooks.json (default
// ~/.codex/hooks.json), which Codex discovers on its own, so config.toml is
// never touched. Codex asks the user to review and trust new hooks once in
// /hooks before they run. See docs/agent/harness.md.
package codex
