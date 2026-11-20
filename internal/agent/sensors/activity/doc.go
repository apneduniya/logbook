// Package activity is the daemon's activity sensor (ADR 0007): every 5
// seconds it reads the OS idle time and the focused application's process
// name through a Probe, classifies the app with focusclass and records a
// wire.ActivitySample. It slows to every 30 seconds while the screen is
// locked, idle time is unknown, or the user has been idle 10 minutes or more.
//
// What it emits:
//   - idle known: a sample with IdleSeconds, FocusApp ("" when unknown or the
//     screen is locked) and FocusKind (focusclass kind, "unknown" when FocusApp is "");
//   - idle unknown (no OS source works, e.g. some Wayland compositors): no
//     sample at all. The daemon then treats activity as unknown and falls back
//     to heartbeat recency; emitting IdleSeconds 0 would claim the user is active.
//
// It never records window titles, keystrokes or anything typed. It does not
// talk to the OS itself (see osprobe) and does not store samples (the Sink does).
package activity
