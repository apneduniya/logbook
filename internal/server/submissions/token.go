package submissions

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// tokenBytes is a review token's randomness: 128 bits (ADR 0020).
const tokenBytes = 16

// tokenLen is the length of a token's text: base64url without padding.
var tokenLen = base64.RawURLEncoding.EncodedLen(tokenBytes)

// newToken returns a fresh review token.
func newToken() string {
	b := make([]byte, tokenBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}

// tokenHash is the stored lookup key of a token.
func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// wellFormedToken reports whether s can be a review token, so a malformed
// one is a plain 404 without a database read.
func wellFormedToken(s string) bool {
	if len(s) != tokenLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == tokenBytes && base64.RawURLEncoding.EncodeToString(b) == s
}

// sealToken encrypts a submission's token, bound to its id.
func (s *Service) sealToken(id uuid.UUID, token string) ([]byte, error) {
	enc, err := s.box.Seal([]byte(token), id[:])
	if err != nil {
		return nil, fmt.Errorf("encrypting review token: %w", err)
	}
	return enc, nil
}

// reviewURL decrypts a submission's token and returns its review link.
func (s *Service) reviewURL(sub db.Submission) (string, error) {
	token, err := s.box.Open(sub.TokenEnc, sub.ID[:])
	if err != nil {
		return "", fmt.Errorf("decrypting review token of %s: %w", sub.ID, err)
	}
	return s.link(string(token)), nil
}

func (s *Service) link(token string) string { return s.reviewBase + "/r/" + token }
