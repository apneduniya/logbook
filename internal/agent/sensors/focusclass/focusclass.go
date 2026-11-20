package focusclass

import (
	"strings"
)

// Kind is the class of a focused application. Its string values are the ones
// wire.ActivitySample.FocusKind accepts.
type Kind string

// The focus kinds.
const (
	Editor   Kind = "editor"
	Terminal Kind = "terminal"
	Browser  Kind = "browser"
	AI       Kind = "ai"
	Other    Kind = "other"
	Unknown  Kind = "unknown"
)

// Operating system names, matching runtime.GOOS.
const (
	osWindows = "windows"
	osDarwin  = "darwin"
	osLinux   = "linux"
	osAny     = ""
)

// linuxCommLen is the length Linux truncates /proc/<pid>/comm to
// (TASK_COMM_LEN 16 minus the terminating NUL).
const linuxCommLen = 15

// Classifier maps application identifiers to kinds for one operating system.
// It is immutable after New and safe for concurrent use.
type Classifier struct {
	goos     string
	exact    map[string]Kind
	prefixes []prefixRule
	// truncated holds Linux rule names longer than linuxCommLen, cut to
	// that length, because /proc/<pid>/comm is cut the same way.
	truncated map[string]Kind
}

type prefixRule struct {
	prefix string
	kind   Kind
}

// New builds a classifier for goos (a runtime.GOOS value). Rules for other
// operating systems are left out, so a generic name like "cmd" or "st" only
// matches where it means a terminal.
func New(goos string) *Classifier {
	c := &Classifier{goos: goos, exact: map[string]Kind{}, truncated: map[string]Kind{}}
	for _, g := range rules() {
		if g.os != osAny && g.os != goos {
			continue
		}
		for _, name := range g.names {
			name = strings.ToLower(name)
			if p, ok := strings.CutSuffix(name, "*"); ok {
				c.prefixes = append(c.prefixes, prefixRule{prefix: p, kind: g.kind})
				continue
			}
			// The first rule for a name wins, so specific rules listed
			// earlier (like the JetBrains Toolbox) beat later ones.
			if _, dup := c.exact[name]; !dup {
				c.exact[name] = g.kind
			}
			if goos == osLinux && len(name) > linuxCommLen {
				if _, dup := c.truncated[name[:linuxCommLen]]; !dup {
					c.truncated[name[:linuxCommLen]] = g.kind
				}
			}
		}
	}
	return c
}

// Classify returns the kind of app, which is a process name (Windows, Linux)
// or a bundle identifier or localized app name (macOS). Matching is
// case-insensitive. An empty name is Unknown; a name not in the table is Other.
func (c *Classifier) Classify(app string) Kind {
	name := c.normalize(app)
	if name == "" {
		return Unknown
	}
	if k, ok := c.exact[name]; ok {
		return k
	}
	// /proc/<pid>/comm cuts names at 15 bytes ("gnome-terminal-server"
	// reads "gnome-terminal-").
	if k, ok := c.truncated[name]; ok && len(name) == linuxCommLen {
		return k
	}
	for _, p := range c.prefixes {
		if strings.HasPrefix(name, p.prefix) {
			return p.kind
		}
	}
	return Other
}

// normalize lowercases the name, keeps only its base name and, on Windows,
// drops the ".exe" extension.
func (c *Classifier) normalize(app string) string {
	name := strings.ToLower(strings.TrimSpace(app))
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if c.goos == osWindows {
		name = strings.TrimSuffix(name, ".exe")
	}
	return name
}
