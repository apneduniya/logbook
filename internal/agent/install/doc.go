// Package install runs `logbook install` and `logbook uninstall` (ADR
// 0009): it puts the agent config, the binary, the .wakatime.cfg redirect,
// the AI harness hooks and the autostart entry in place, in that order,
// rolls back what it changed when a step fails, and reverses everything on
// uninstall.
//
// It owns the order of the steps, the rollback rules and what each step
// reports. The steps themselves live in the subpackages (wakacfg,
// autostart, selfinstall, keycheck) and in internal/agent/harness, which it
// reaches only through the small interfaces defined here.
package install
