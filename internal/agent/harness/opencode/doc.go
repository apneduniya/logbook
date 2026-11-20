// Package opencode connects OpenCode to Logbook. OpenCode has no
// command-hook config; it has JavaScript plugins. Logbook installs one small
// plugin file, <config>/opencode/plugins/logbook.js, which OpenCode loads
// automatically from its global plugin directory. The plugin forwards
// metadata to `logbook hook opencode <event>`; Parse maps that to AI events.
// See docs/agent/harness.md.
package opencode
