package aievent

import (
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/apneduniya/logbook/pkg/wire"
)

// Field limits, matching wire.AIEvent.Validate.
const (
	maxID    = 256
	maxShort = 256
	maxCwd   = 1024
)

// Fields is what a harness parser extracts from one hook payload.
type Fields struct {
	Kind      wire.AIEventKind
	SessionID string
	Cwd       string
	Model     string
	Tool      string
	// PromptChars is the prompt's length in runes (prompt_submit only).
	PromptChars int
	// PromptDigest is the prompt's unkeyed digest (prompt_submit only). The
	// hook keys it with the device's prompt key (see PromptHash); it is
	// never recorded as is.
	PromptDigest PromptDigest
	// Decision is "allow", "deny" or "".
	Decision string
	// Ago is how long before the hook ran the event happened. It is zero
	// except where a harness reports a duration instead of a start hook (see
	// package cursor). Values outside (0, MaxAgo] are ignored.
	Ago time.Duration
}

// MaxAgo bounds Fields.Ago, so a bogus duration can't move an event far into
// the past.
const MaxAgo = 6 * time.Hour

// ProjectFunc names the project a working directory belongs to.
type ProjectFunc func(cwd string) string

// Build assembles the wire event. Text fields are cleaned of control
// characters and cut to the wire limits; a cwd too long to keep is dropped
// rather than cut, since a truncated path would name the wrong directory.
func Build(h wire.Harness, f Fields, now time.Time, project ProjectFunc) wire.AIEvent {
	at := now
	if f.Ago > 0 && f.Ago <= MaxAgo {
		at = now.Add(-f.Ago)
	}
	ev := wire.AIEvent{
		Time:      at.UTC(),
		Harness:   h,
		Kind:      f.Kind,
		SessionID: Clean(f.SessionID, maxID),
		Model:     Clean(f.Model, maxShort),
		Tool:      Clean(f.Tool, maxShort),
	}
	if cwd := strings.TrimSpace(f.Cwd); cwd != "" && len(cwd) <= maxCwd && utf8.ValidString(cwd) && !hasControl(cwd) {
		ev.Cwd = cwd
		ev.Project = Clean(project(cwd), maxShort)
	}
	if f.Kind == wire.AIPromptSubmit && f.PromptChars > 0 {
		ev.PromptChars = f.PromptChars
	}
	if f.Decision == "allow" || f.Decision == "deny" {
		ev.Decision = f.Decision
	}
	return ev
}

// Clean removes control characters and invalid UTF-8 from s and cuts it to
// at most maxBytes bytes on a rune boundary.
func Clean(s string, maxBytes int) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if hasControl(s) {
		s = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, s)
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

// Runes returns the length of s in characters (runes), the unit PromptChars
// uses. The text itself is not kept.
func Runes(s string) int { return utf8.RuneCountInString(s) }

// StatFunc reports whether a path exists. It is os.Stat-shaped so callers can
// pass a fake in tests.
type StatFunc func(path string) bool

// maxWalk bounds how many parent directories ProjectName climbs.
const maxWalk = 64

// ProjectName returns the name of the git repository containing cwd (the
// directory holding .git, which may be a file in worktrees and submodules),
// or cwd's own base name when it isn't inside one.
func ProjectName(cwd string, exists StatFunc) string {
	dir := filepath.Clean(cwd)
	for range maxWalk {
		if exists(filepath.Join(dir, ".git")) {
			return dirName(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dirName(filepath.Clean(cwd))
}

// dirName is filepath.Base without the separator or volume results for a
// root directory.
func dirName(dir string) string {
	name := filepath.Base(dir)
	if name == string(filepath.Separator) || name == "." || strings.HasSuffix(name, ":") || name == filepath.VolumeName(dir)+string(filepath.Separator) {
		return ""
	}
	return name
}

// EventKind pairs a harness's hook event name with the AI event kind it
// becomes.
type EventKind struct {
	Event string
	Kind  wire.AIEventKind
}

// Table lists the events a harness installs, in install order.
type Table []EventKind

// Kind returns the AI event kind for a harness event name.
func (t Table) Kind(event string) (wire.AIEventKind, bool) {
	for _, e := range t {
		if e.Event == event {
			return e.Kind, true
		}
	}
	return "", false
}

// Names returns the event names in order.
func (t Table) Names() []string {
	out := make([]string, len(t))
	for i, e := range t {
		out[i] = e.Event
	}
	return out
}
