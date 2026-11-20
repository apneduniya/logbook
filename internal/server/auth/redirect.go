package auth

import (
	"net/url"
	"strings"
)

const maxNextLen = 512

// safeNext returns next if it is a same-origin path (with optional query),
// and "/" otherwise. It rejects absolute URLs, scheme-relative "//host",
// backslashes (browsers treat "/\host" as "//host"), control characters and
// anything that parses with a scheme or host.
func safeNext(next string) string {
	if next == "" || len(next) > maxNextLen || next[0] != '/' {
		return "/"
	}
	if strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\") {
		return "/"
	}
	for _, r := range next {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") {
		return "/"
	}
	// Re-encode from parsed parts so the Location header is canonical.
	out := &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	s := out.String()
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return "/"
	}
	return s
}
