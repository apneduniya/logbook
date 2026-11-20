// Package selfinstall copies the running logbook binary to a stable,
// per-user location so autostart entries and harness hooks can point at a
// path that survives the download folder being cleaned:
//
//   - Windows: %LOCALAPPDATA%\Programs\Logbook\logbook.exe
//   - macOS:   ~/Library/Application Support/Logbook/bin/logbook
//   - Linux:   ~/.local/bin/logbook
//
// Replacing is atomic and never needs admin rights. On Windows a running
// executable cannot be overwritten or deleted but can be renamed, so the
// old one is renamed aside first and cleaned up later. The package does not
// start or stop the daemon; autostart does.
package selfinstall
