package httpapi

import (
	"net/http"
	"strings"
)

// CORS policy for the browser API (ADR 0011). The web UI is served from its
// own origin and calls the API directly with credentials, so exactly that
// origin is allowed. Session-authenticated requests that change state also
// need X-CSRF-Token, which a page on another origin can't send without
// passing this check.
const (
	corsAllowMethods  = "GET, POST, PATCH, DELETE"
	corsAllowHeaders  = "Content-Type, X-CSRF-Token"
	corsExposeHeaders = "Retry-After, X-Request-Id"
	corsMaxAge        = "600" // seconds browsers may cache a preflight
	corsPathPrefix    = "/api/"
)

// corsMethods and corsHeaders are the allowed values in lower case, for
// matching preflight requests.
var (
	corsMethods = map[string]bool{"GET": true, "POST": true, "PATCH": true, "DELETE": true}
	corsHeaders = map[string]bool{"content-type": true, "x-csrf-token": true}
)

// CORSConfig configures CORS.
type CORSConfig struct {
	// Origin is the single allowed browser origin, such as
	// "https://logbook.boogles.dev": scheme, host and optional port, no path.
	Origin string
	// Skip lists path prefixes under /api/ that are never called from a
	// browser (heartbeat ingest, agent uploads, the program API). They get
	// no CORS headers. An entry with a "{name}" segment matches whole
	// segments, "{name}" standing for any one, so "/api/v1/users/{id}/stats"
	// skips every user's stats but not "/api/v1/users/{id}/profile".
	Skip []string
}

// CORS returns middleware that lets the configured origin call /api/* with
// credentials. Other paths (/auth/*, /healthz) and the Skip prefixes are
// passed through untouched. Preflight requests are answered here: 204 when
// the origin, method and headers are all allowed, 403 otherwise. Actual
// requests from other origins are served without CORS headers, so the
// browser keeps the response from the calling page.
func CORS(c CORSConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !corsApplies(r.URL.Path, c.Skip) {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			allowed := c.Origin != "" && origin == c.Origin

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				if !allowed || !preflightAllowed(r) {
					WriteError(w, r, Errorf(http.StatusForbidden, nil, "CORS request not allowed"))
					return
				}
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Methods", corsAllowMethods)
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
				h.Set("Access-Control-Max-Age", corsMaxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if allowed {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func corsApplies(path string, skip []string) bool {
	if !strings.HasPrefix(path, corsPathPrefix) {
		return false
	}
	for _, p := range skip {
		if skipMatches(path, p) {
			return false
		}
	}
	return true
}

// skipMatches reports whether path is covered by the Skip entry prefix.
// Without a "{name}" segment that is a plain string prefix. With one, the
// entry is compared segment by segment: "{name}" matches any non-empty
// segment, every other segment must be equal, and path may go on after the
// last one.
func skipMatches(path, prefix string) bool {
	if !strings.Contains(prefix, "{") {
		return strings.HasPrefix(path, prefix)
	}
	want := strings.Split(prefix, "/")
	got := strings.Split(path, "/")
	if len(got) < len(want) {
		return false
	}
	for i, w := range want {
		wild := strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}")
		if (wild && got[i] == "") || (!wild && got[i] != w) {
			return false
		}
	}
	return true
}

// preflightAllowed checks the requested method and headers against the
// allow-lists. Header names are case-insensitive and comma-separated, and
// may be split across several header lines.
func preflightAllowed(r *http.Request) bool {
	if !corsMethods[r.Header.Get("Access-Control-Request-Method")] {
		return false
	}
	for _, line := range r.Header.Values("Access-Control-Request-Headers") {
		for name := range strings.SplitSeq(line, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" && !corsHeaders[name] {
				return false
			}
		}
	}
	return true
}
