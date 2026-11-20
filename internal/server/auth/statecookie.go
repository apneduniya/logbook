package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	stateCookieName = "logbook_oauth"
	stateCookiePath = "/auth/"
	stateTTL        = 10 * time.Minute
)

var errBadState = errors.New("invalid or expired sign-in state")

// oauthState is what we remember between /auth/login and /auth/callback. It
// lives only in a short-lived HttpOnly cookie, MAC'd so it can't be forged.
type oauthState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Next     string `json:"n"`
	Expires  int64  `json:"e"` // unix seconds
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Service) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.stateKey)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// encodeState serialises and MACs st as "payload.mac", both base64url.
func (s *Service) encodeState(st oauthState) (string, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("encoding oauth state: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload)), nil
}

// decodeState verifies the MAC and expiry of a cookie value.
func (s *Service) decodeState(v string) (oauthState, error) {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok {
		return oauthState{}, errBadState
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(payload)) {
		return oauthState{}, errBadState
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return oauthState{}, errBadState
	}
	var st oauthState
	if err := json.Unmarshal(b, &st); err != nil {
		return oauthState{}, errBadState
	}
	if st.State == "" || st.Verifier == "" || s.now().Unix() > st.Expires {
		return oauthState{}, errBadState
	}
	return st, nil
}

func (s *Service) setStateCookie(w http.ResponseWriter, value string) {
	//nolint:gosec // G124: Secure is on unless LOGBOOK_DEV_INSECURE_COOKIE is set for plain-http dev
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    value,
		Path:     stateCookiePath,
		MaxAge:   int(stateTTL / time.Second),
		HttpOnly: true,
		Secure:   s.secure,
		// Lax so the cookie comes back on HCA's top-level redirect to us.
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearStateCookie(w http.ResponseWriter) {
	//nolint:gosec // G124: Secure is on unless LOGBOOK_DEV_INSECURE_COOKIE is set for plain-http dev
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     stateCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}
