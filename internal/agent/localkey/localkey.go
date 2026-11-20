package localkey

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// HKDF info strings. Changing one changes every derived value, so a new
// derivation gets a new version suffix instead.
const (
	promptInfo = "logbook prompt hash v1"
	otlpInfo   = "logbook otlp token v1"
)

// keyLen is the length of every derived key, in bytes.
const keyLen = 32

// minToken matches config.Config.Validate: a local token shorter than this
// is not a usable secret.
const minToken = 32

// ErrNoToken means the local token is missing or too short to derive from.
var ErrNoToken = errors.New("local token is missing or too short")

// PromptKey returns the 32-byte HMAC key for prompt hashes:
// HKDF-SHA256(secret = localToken, salt = none, info = "logbook prompt hash v1").
func PromptKey(localToken string) ([]byte, error) {
	return derive(localToken, promptInfo)
}

// OTLPToken returns the token Claude Code's exporter sends in the
// X-Logbook-Token header: hex(HKDF-SHA256(secret = localToken, salt = none,
// info = "logbook otlp token v1")), 64 hex characters.
func OTLPToken(localToken string) (string, error) {
	k, err := derive(localToken, otlpInfo)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(k), nil
}

func derive(localToken, info string) ([]byte, error) {
	if len(localToken) < minToken {
		return nil, ErrNoToken
	}
	k, err := hkdf.Key(sha256.New, []byte(localToken), nil, info, keyLen)
	if err != nil {
		return nil, fmt.Errorf("deriving %q: %w", info, err)
	}
	return k, nil
}
