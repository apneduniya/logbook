package auth

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// usernameRe mirrors the users.username CHECK constraint.
var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,39}$`)

const (
	// maxBaseUsername leaves room for a numeric suffix within 39 characters.
	maxBaseUsername  = 32
	fallbackUsername = "hacker"
)

// sanitizeUsername lowercases s and keeps only characters the users table
// allows. Whitespace, dots and plus signs become hyphens; runs of hyphens
// collapse and leading/trailing separators are trimmed. It returns "" when
// fewer than two characters survive.
func sanitizeUsername(s string) string {
	var b strings.Builder
	lastHyphen := true // suppress leading hyphens
	for _, r := range strings.ToLower(s) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r == '_':
			b.WriteRune(r)
			lastHyphen = false
		case r == '-' || r == '.' || r == '+' || unicode.IsSpace(r):
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
		if b.Len() >= maxBaseUsername {
			break
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > maxBaseUsername {
		out = strings.Trim(out[:maxBaseUsername], "-_")
	}
	if len(out) < 2 {
		return ""
	}
	return out
}

// baseUsername picks a username from an HCA identity: the name if granted
// and usable, else a fixed fallback (which upsertUser gives a random
// suffix). It never uses the email address: usernames are public and
// permanent, and THREAT-MODEL.md keeps email private.
func baseUsername(id hcaIdentity) string {
	if u := sanitizeUsername(id.FirstName + " " + id.LastName); u != "" {
		return u
	}
	return fallbackUsername
}

// usernameCandidate returns the n-th try for base: base, base2, base3, ...
func usernameCandidate(base string, n int) string {
	if n == 0 {
		return base
	}
	return base + strconv.Itoa(n+1)
}
