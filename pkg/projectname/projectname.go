package projectname

import (
	"strings"
	"unicode"
)

// Placeholder is what some WakaTime clients send instead of a project name
// ("use whatever I sent last"). It is stored as "" (no project).
const Placeholder = "<<LAST_PROJECT>>"

// Normalize removes control characters, trims surrounding white space and
// turns Placeholder into "". It never fails; callers check the length of
// the result.
func Normalize(p string) string {
	p = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, p))
	if p == Placeholder {
		return ""
	}
	return p
}
