package auth

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/apneduniya/logbook/internal/server/config"
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

// originOf turns the configured web URL into an origin with
// config.ParseOrigin, the function CORS uses too, so the two always agree.
// Empty stays empty (the UI shares the API's origin). ParseOrigin rejects
// anything after the host, so the origin can be joined with a safeNext path
// without surprises.
func originOf(webURL string) (string, error) {
	if webURL == "" {
		return "", nil
	}
	origin, err := config.ParseOrigin(webURL)
	if err != nil {
		return "", fmt.Errorf("auth: web URL: %w", err)
	}
	return origin, nil
}

// webLocation is where to send the browser for path, which must be a
// safeNext result or a fixed path of ours: the web origin plus path, or path
// alone when the UI shares the API's origin. safeNext guarantees path starts
// with exactly one "/", so the result can never name another host.
func (s *Service) webLocation(path string) string {
	return s.webOrigin + path
}
