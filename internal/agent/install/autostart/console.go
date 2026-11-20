package autostart

// DetachedEnv is set to "1" in the environment of a daemon that already
// runs without a visible console, so it never relaunches itself again.
const DetachedEnv = "LOGBOOK_DETACHED"

// needsRelaunch decides whether `logbook run` should restart itself
// without a console window. At login Windows starts the HKCU Run value as
// a console program, and Explorer gives it a new, visible console (or a
// Windows Terminal tab) that it alone is attached to; closing that window
// would kill the daemon. A daemon started from a terminal the user opened
// shares that console with the shell (two or more processes) and stays in
// the foreground, and one started by Start, or by an earlier relaunch,
// carries DetachedEnv.
//
// consoleProcs is GetConsoleProcessList's count: 0 means no console at all
// (nothing to hide), 1 means this process owns its console.
func needsRelaunch(detachedEnv string, consoleProcs int) bool {
	return detachedEnv != "1" && consoleProcs == 1
}
