// Package osprobe reads the operating system's input idle time, the focused
// application's process name (or macOS bundle id) and the screen-lock state,
// without cgo (ADR 0007, ADR 0009).
//
// Each OS has its own Probe in a build-tagged file:
//   - Windows: user32/kernel32 through golang.org/x/sys/windows lazy DLLs;
//   - macOS: CoreGraphics, CoreFoundation and the Objective-C runtime through
//     github.com/ebitengine/purego;
//   - Linux: D-Bus (Mutter, org.freedesktop.ScreenSaver, logind) through
//     github.com/godbus/dbus/v5 and X11 through github.com/jezek/xgb.
//
// The OS-independent logic (Windows tick arithmetic, the Linux idle-source
// order and fallback chain) lives in untagged files so it is tested on every
// platform. The package never reads window titles, keystrokes or any content;
// it does not decide what a reading means, which is the activity sensor's job.
package osprobe
