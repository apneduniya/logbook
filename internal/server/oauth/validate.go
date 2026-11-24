package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Field limits for apps.
const (
	MaxNameLen          = 64
	MaxRedirectURIs     = 5
	MaxURLLen           = 2048
	maxStateLen         = 2048
	minVerifierLen      = 43
	maxVerifierLen      = 128
	challengeMethodS256 = "S256"
)

var (
	slugRe = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)
	// pkceRe is RFC 7636's unreserved characters for code_verifier and
	// code_challenge.
	pkceRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

// scopeOrder is the canonical order scopes are stored and returned in.
var scopeOrder = []string{ScopeProfile, ScopeRead, ScopeSubmissions}

// userScopes are the scopes a user can grant through the authorize flow.
var userScopes = []string{ScopeProfile, ScopeRead}

// parseScopes splits a space-separated scope parameter, keeps only scopes
// in allowed, and returns them deduplicated in canonical order. An empty
// parameter means def. ok is false if any scope isn't allowed.
func parseScopes(raw string, allowed []string, def string) (scopes []string, ok bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		fields = []string{def}
	}
	for _, f := range fields {
		if !slices.Contains(allowed, f) {
			return nil, false
		}
	}
	return canonicalScopes(fields), true
}

// canonicalScopes deduplicates scopes into canonical order, dropping
// unknown ones.
func canonicalScopes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range scopeOrder {
		if slices.Contains(in, s) {
			out = append(out, s)
		}
	}
	return out
}

// covers reports whether have includes every scope in want.
func covers(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// scopeString is the space-separated form used on the wire.
func scopeString(scopes []string) string { return strings.Join(scopes, " ") }

// cleanName trims s and checks it is 1..MaxNameLen characters of valid
// UTF-8 with no control or bidirectional-override characters.
func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxNameLen {
		return "", fmt.Errorf("%w: name must be 1-%d characters", ErrInvalid, MaxNameLen)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == unicode.ReplacementChar {
			return "", fmt.Errorf("%w: name must not contain control characters", ErrInvalid)
		}
	}
	return s, nil
}

func checkSlug(s string) error {
	if !slugRe.MatchString(s) {
		return fmt.Errorf("%w: slug must be 2-40 characters of a-z, 0-9 and -", ErrInvalid)
	}
	return nil
}

// checkRedirectURIs validates 1..MaxRedirectURIs redirect URIs and drops
// exact duplicates. Each is stored exactly as given; matching is exact.
func checkRedirectURIs(uris []string) ([]string, error) {
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		if slices.Contains(out, u) {
			continue
		}
		if err := checkRedirectURI(u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if len(out) < 1 || len(out) > MaxRedirectURIs {
		return nil, fmt.Errorf("%w: redirect_uris must have 1-%d entries", ErrInvalid, MaxRedirectURIs)
	}
	return out, nil
}

// checkRedirectURI accepts an absolute https URL, or plain http on
// localhost or 127.0.0.1 with any port, with no user info or fragment.
func checkRedirectURI(raw string) error {
	if !webURLOK(raw) || strings.Contains(raw, "#") {
		return fmt.Errorf("%w: redirect URI %q must be an absolute https URL (or http://localhost or http://127.0.0.1) with no fragment",
			ErrInvalid, truncate(raw, 100))
	}
	return nil
}

// checkHomepage accepts "" or a URL that checkRedirectURI's scheme rules allow.
func checkHomepage(raw string) error {
	if raw == "" || webURLOK(raw) {
		return nil
	}
	return fmt.Errorf("%w: homepage_url must be an absolute https URL", ErrInvalid)
}

func webURLOK(raw string) bool {
	if raw == "" || len(raw) > MaxURLLen || strings.ContainsAny(raw, " \t\r\n\\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1"
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// validChallenge checks a code_challenge: S256 output is 43 base64url
// characters, but RFC 7636 allows 43-128 unreserved characters.
func validChallenge(c string) bool {
	return len(c) >= minVerifierLen && len(c) <= maxVerifierLen && pkceRe.MatchString(c)
}

// verifyPKCE checks verifier against an S256 challenge (RFC 7636 §4.6).
func verifyPKCE(challenge, verifier string) bool {
	if len(verifier) < minVerifierLen || len(verifier) > maxVerifierLen || !pkceRe.MatchString(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(want), []byte(challenge)) == 1
}
