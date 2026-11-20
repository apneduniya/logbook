// Package autostart starts the Logbook daemon (`logbook run`) at login and
// can start it right away, without admin rights (ADR 0009):
//
//   - Windows: a value in HKCU\Software\Microsoft\Windows\CurrentVersion\Run,
//     and a hidden, detached process for Start. Windows gives the Run entry
//     a visible console at login; Background, called first by `logbook run`,
//     moves the daemon into a hidden process;
//   - macOS: a LaunchAgent plist in ~/Library/LaunchAgents, loaded with
//     launchctl bootstrap gui/<uid>;
//   - Linux: a systemd --user unit, or an XDG autostart .desktop file when
//     the user has no systemd user manager.
//
// The text of each entry is built by pure functions (files.go) so every
// platform's output is tested on any OS. Commands and the registry are
// behind small interfaces. The package does not install the binary or
// decide where it lives; selfinstall does.
package autostart
