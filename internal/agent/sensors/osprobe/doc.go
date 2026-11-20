// Package osprobe reads the operating system's input idle time, the focused
// application's process name (or macOS bundle id) and the screen-lock state,
// without cgo (ADR 0007, ADR 0009).
//
// Each OS has its own Probe in a build-tagged file:
//   - Windows: user32/kernel32 through golang.org/x/sys/windows lazy DLLs;
//   - macOS: CoreGraphics, CoreFoundation and the Objective-C runtime through
//     github.com/ebitengine/purego;
//   - Linux: D-Bus (Mutter, org.freedesktop.ScreenSaver, logind) through
//     github.com/godbus/dbus/v5, Wayland ext-idle-notify-v1 through a small
//     built-in wire-protocol client, and X11 through github.com/jezek/xgb.
//
// Pure logic is split from the OS calls so it is unit-tested with fakes: the
// Windows tick arithmetic and the idle-source chain in untagged files, the
// Linux session detection, source gating, logind trust rule, Wayland codec
// and X11 PID choice in Linux files. The package never reads window titles,
// keystrokes or any content; it does not decide what a reading means, which
// is the activity sensor's job.
package osprobe
