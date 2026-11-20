package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// String returns the agent's version: the module version for tagged builds,
// the short VCS revision for development builds, or "dev".
func String() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return "dev-" + s.Value[:7]
		}
	}
	return "dev"
}

// UserAgent returns "logbook-agent/<version> (<os>-<arch>)".
func UserAgent() string {
	return fmt.Sprintf("logbook-agent/%s (%s-%s)", String(), runtime.GOOS, runtime.GOARCH)
}
