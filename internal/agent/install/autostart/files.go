package autostart

import (
	"bytes"
	"encoding/xml"
	"strings"
)

// Label is the launchd job label and the base name of every entry.
const Label = "dev.boogles.logbook"

// runArg is the daemon subcommand every entry starts.
const runArg = "run"

// LaunchdPlist returns the LaunchAgent for binPath. The job starts at
// login, is restarted only when it exits unsuccessfully (a crash), runs as
// a background process (lower CPU and I/O priority) and sends stdout and
// stderr to logPath; the daemon's own log is agent.log.
func LaunchdPlist(binPath, logPath string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(binPath) + `</string>
		<string>` + runArg + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(logPath) + `</string>
</dict>
</plist>
`)
	return b.Bytes()
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails to write
	return b.String()
}

// SystemdUnit returns the systemd --user service for binPath. It restarts
// only on failure, is capped at 64 MB of memory and 10% of one CPU, and
// runs at a lower priority so editors stay responsive.
//
// With graphical set, the unit belongs to the desktop session: it is
// wanted by graphical-session.target, ordered after it and stopped with it
// (PartOf). Desktops import DISPLAY, WAYLAND_DISPLAY and the session bus
// into the user manager before starting that target, so the idle and focus
// sensors see them. Ordering alone (After=) would not do this for a unit
// pulled in by default.target, which the user manager reaches at first
// login, before the desktop has started. Without a graphical session
// (SSH-only, WSL, desktops without systemd integration) the unit is wanted
// by default.target instead.
func SystemdUnit(binPath string, graphical bool) []byte {
	unit, wantedBy := "", "default.target"
	if graphical {
		unit = "PartOf=graphical-session.target\nAfter=graphical-session.target\n"
		wantedBy = "graphical-session.target"
	}
	return []byte(`[Unit]
Description=Logbook agent (passive time tracker)
Documentation=https://github.com/apneduniya/logbook
` + unit + `
[Service]
Type=simple
ExecStart=` + systemdQuote(binPath) + ` ` + runArg + `
Restart=on-failure
RestartSec=10
MemoryMax=64M
CPUQuota=10%
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7

[Install]
WantedBy=` + wantedBy + `
`)
}

// systemdQuote quotes one ExecStart word: inside double quotes, backslash
// and quote are escaped, and specifier '%' and variable '$' are doubled.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

// DesktopEntry returns the XDG autostart entry for binPath, used when there
// is no systemd user manager.
func DesktopEntry(binPath string) []byte {
	return []byte(`[Desktop Entry]
Type=Application
Name=Logbook
Comment=Logbook agent (passive time tracker)
Exec=` + desktopQuote(binPath) + ` ` + runArg + `
Terminal=false
NoDisplay=true
X-GNOME-Autostart-enabled=true
`)
}

// desktopQuote quotes one Exec argument per the Desktop Entry spec: inside
// double quotes, `"`, '`', '$' and '\' get a backslash, '%' is doubled, and
// then the whole value is escaped as a string, which doubles backslashes.
func desktopQuote(s string) string {
	arg := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`, `%`, `%%`).Replace(s)
	return strings.ReplaceAll(`"`+arg+`"`, `\`, `\\`)
}

// WindowsRunValue returns the Run registry value for binPath. Windows
// parses it as a command line, so the path is quoted (it may contain
// spaces). Paths cannot contain '"' on Windows.
func WindowsRunValue(binPath string) string {
	return `"` + binPath + `" ` + runArg
}
