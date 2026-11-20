package selfinstall

import (
	"errors"
	"strings"
)

// Target returns where the binary is installed for goos. home is the user's
// home directory; localAppData is %LOCALAPPDATA% (Windows only; when empty
// it defaults to home\AppData\Local). Paths use goos's separator, so the
// logic is testable for every OS from any OS.
func Target(goos, home, localAppData string) (string, error) {
	if home == "" && (goos != "windows" || localAppData == "") {
		return "", errors.New("no home directory")
	}
	switch goos {
	case "windows":
		if localAppData == "" {
			localAppData = join(goos, home, "AppData", "Local")
		}
		return join(goos, localAppData, "Programs", "Logbook", "logbook.exe"), nil
	case "darwin":
		return join(goos, home, "Library", "Application Support", "Logbook", "bin", "logbook"), nil
	default:
		return join(goos, home, ".local", "bin", "logbook"), nil
	}
}

func join(goos string, parts ...string) string {
	sep := "/"
	if goos == "windows" {
		sep = `\`
	}
	for i, p := range parts {
		if i > 0 {
			p = strings.Trim(p, `/\`)
		} else {
			p = strings.TrimRight(p, `/\`)
		}
		parts[i] = p
	}
	return strings.Join(parts, sep)
}
