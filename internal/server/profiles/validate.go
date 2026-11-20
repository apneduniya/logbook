package profiles

import (
	"errors"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // timezone validation must not depend on the host's zoneinfo
	"unicode"
	"unicode/utf8"
)

// Limits for profile fields.
const (
	MaxDisplayNameRunes = 64
	maxTimezoneLen      = 64
)

// Validation errors; their messages are safe to show clients.
var (
	ErrInvalidDisplayName = errors.New("display_name must be at most 64 characters with no control characters")
	ErrInvalidTimezone    = errors.New("timezone must be an IANA time zone such as Asia/Kolkata")
	ErrEmptyPatch         = errors.New("no fields to update")
)

// usernamePattern mirrors the users.username CHECK constraint.
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,39}$`)

// ValidUsername reports whether s could be a username.
func ValidUsername(s string) bool { return usernamePattern.MatchString(s) }

// NormalizeDisplayName trims s and checks it is short, valid UTF-8 and free
// of control and bidirectional override characters (which can spoof names).
func NormalizeDisplayName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxDisplayNameRunes {
		return "", ErrInvalidDisplayName
	}
	for _, r := range s {
		if unicode.IsControl(r) || isBidiControl(r) {
			return "", ErrInvalidDisplayName
		}
	}
	return s, nil
}

func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F
}

// ValidateTimezone checks name is an IANA zone Go can load. "Local" and ""
// are rejected because they depend on the host, not the user.
func ValidateTimezone(name string) error {
	if name == "" || name == "Local" || len(name) > maxTimezoneLen {
		return ErrInvalidTimezone
	}
	if _, err := time.LoadLocation(name); err != nil {
		return ErrInvalidTimezone
	}
	return nil
}
