// Package wakacfg points the user's WakaTime config (.wakatime.cfg) at the
// local Logbook daemon and puts it back on uninstall (ADR 0005, ADR 0009).
//
// It owns: finding the file the way wakatime-cli does, editing only
// settings.api_url and settings.api_key while keeping every other byte, the
// one-time backup in the data directory, the install state file, and the
// rule for which server to mirror heartbeats to. It does not write the agent
// config or talk to any server; the install command does that with the
// values this package returns.
package wakacfg
