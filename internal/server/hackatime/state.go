package hackatime

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/apneduniya/logbook/internal/server/secretbox"
)

// stateTTL bounds how long a user has to finish the Hackatime consent screen.
const stateTTL = 10 * time.Minute

// ErrBadState means the callback's state doesn't match the one we issued to
// this user, or the link request expired.
var ErrBadState = errors.New("hackatime: invalid or expired link request")

// linkState is what the browser carries between /link and /callback, sealed
// in a cookie so it can't be read or forged.
type linkState struct {
	State    string    `json:"s"`
	Verifier string    `json:"v"`
	UserID   uuid.UUID `json:"u"`
	Expires  int64     `json:"e"`
}

func stateAAD(userID uuid.UUID) []byte {
	return []byte("hackatime-link-state:" + userID.String())
}

// newLinkState creates a fresh state and PKCE verifier for userID and returns
// it with its sealed cookie value.
func newLinkState(box *secretbox.Box, userID uuid.UUID, now time.Time) (linkState, string, error) {
	var b [32]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	st := linkState{
		State:    base64.RawURLEncoding.EncodeToString(b[:]),
		Verifier: oauth2.GenerateVerifier(),
		UserID:   userID,
		Expires:  now.Add(stateTTL).Unix(),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return linkState{}, "", fmt.Errorf("encoding link state: %w", err)
	}
	sealed, err := box.Seal(raw, stateAAD(userID))
	if err != nil {
		return linkState{}, "", fmt.Errorf("sealing link state: %w", err)
	}
	return st, base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openLinkState checks the cookie belongs to userID, hasn't expired, and
// matches the state echoed back by Hackatime. It returns the PKCE verifier.
func openLinkState(box *secretbox.Box, cookie string, userID uuid.UUID, gotState string, now time.Time) (string, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil || cookie == "" || gotState == "" {
		return "", ErrBadState
	}
	raw, err := box.Open(sealed, stateAAD(userID))
	if err != nil {
		return "", ErrBadState
	}
	var st linkState
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", ErrBadState
	}
	if st.UserID != userID || now.Unix() > st.Expires ||
		subtle.ConstantTimeCompare([]byte(st.State), []byte(gotState)) != 1 {
		return "", ErrBadState
	}
	return st.Verifier, nil
}
