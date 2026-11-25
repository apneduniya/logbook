package programapi

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// timeLayouts are the forms ParseTime accepts, standing in for Rails'
// Time.zone.parse in the UTC zone: dates (midnight UTC), RFC 3339, and
// date-times without a zone (UTC), with "T" or a space. An offset may have
// a colon or not (+05:30, +0530), and a space may come before it or a "UTC"
// suffix: Ruby's Time#to_s ("2026-06-10 10:00:00 UTC", "... +0000") is what
// Ruby clients send when they put a Time in a query.
var timeLayouts = []string{
	"2006-01-02",
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05.999999999 UTC",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999 Z07:00",
	"2006-01-02 15:04:05.999999999Z0700",
	"2006-01-02 15:04:05.999999999 Z0700",
	"2006-01-02 15:04:05.999999999 UTC",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
}

// ParseTime parses a start_date/end_date style value. ok is false when the
// value isn't a time Hackatime would accept.
func ParseTime(s string) (t time.Time, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// timeParam reads a time parameter. present is false when it is absent or
// blank (Rails' blank?), so the caller uses its default; ok is false when it
// is present but invalid.
func timeParam(q url.Values, name string) (t time.Time, present, ok bool) {
	raw := q.Get(name)
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, false, true
	}
	t, ok = ParseTime(raw)
	return t, true, ok
}

// firstTime reads the first present parameter among names that parses,
// falling back to def, as ProjectStatsQuery#parse_time does (an unparsable
// value is skipped, never an error).
func firstTime(q url.Values, def time.Time, names ...string) time.Time {
	for _, n := range names {
		if t, present, ok := timeParam(q, n); present && ok {
			return t
		}
	}
	return def
}

// listParam reads a comma-separated list the way Hackatime does
// (params[name].presence&.split(",")): nil (no filter) when absent or blank.
// Ruby's split drops trailing empty strings but keeps inner ones, so a value
// of only commas is an empty, non-nil list that matches nothing, as
// where(project: []) does. The array form name[]=a&name[]=b is accepted too
// (Rails splits an array on "," elements, which Stardance relies on). An
// array of only "," elements is likewise an empty filter; one of only blank
// elements is no filter (Rails munges a bare name[] to an empty array).
func listParam(q url.Values, name string) []string {
	if vs, ok := q[name+"[]"]; ok && len(q.Get(name)) == 0 {
		var out []string
		for _, v := range vs {
			switch v {
			case "":
			case ",":
				if out == nil {
					out = []string{}
				}
			default:
				out = append(out, v)
			}
		}
		return out
	}
	raw := q.Get(name)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// trimmedList is listParam with each item trimmed and blanks and duplicates
// dropped, as ProjectStatsQuery parses its projects parameter.
func trimmedList(q url.Values, name string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range listParam(q, name) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// validList reports whether every name is valid UTF-8 and short enough to be
// a project or category name.
func validList(names []string) bool {
	for _, n := range names {
		if len(n) > maxProjectName || !utf8.ValidString(n) {
			return false
		}
	}
	return true
}

// maxProjectName matches the wire limit on project names.
const maxProjectName = 256

// isTrue is Hackatime's params[x] == "true".
func isTrue(q url.Values, name string) bool { return q.Get(name) == "true" }

// rubyToI is String#to_i: the leading optional sign and digits, else 0.
func rubyToI(s string) int {
	s = strings.TrimLeft(s, " \t\n")
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0 // no digits (or overflow): Ruby gives 0 for the former
	}
	return n
}

// identKind says how a path identifier is resolved (docs/api/programs.md).
type identKind int

const (
	identInvalid  identKind = iota // can't name any user
	identMy                        // the caller's own user
	identNumeric                   // public_id, then a numeric username
	identUsername                  // a username, case-insensitive
)

// classifyIdent decides how to resolve a path identifier. allowMy is true
// only on the stats-controller endpoints.
func classifyIdent(id string, allowMy bool) identKind {
	switch {
	case id == "":
		return identInvalid
	case id == "my" && allowMy:
		return identMy
	case isDigits(id):
		return identNumeric
	case usernameOK(id):
		return identUsername
	default:
		return identInvalid
	}
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// usernamePattern mirrors the users.username CHECK constraint.
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,39}$`)

// usernameOK reports whether s could be a username.
func usernameOK(s string) bool { return usernamePattern.MatchString(s) }
