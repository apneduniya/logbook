// Package doctor turns facts gathered about the agent's setup into findings
// with actionable fixes, for `logbook doctor`. Diagnose is pure: the command
// gathers the facts (config, server check, daemon status, .wakatime.cfg) and
// passes them in, so every rule is unit-tested without touching the machine.
// It never includes keys or tokens in its output.
package doctor
