// Package evidence defines Logbook's tamper-evident evidence chain (ADR 0010):
// the canonical encoding of a day's raw events, the chain hash that links
// days, and ed25519 signing and verification. Agent and server both use it,
// so the formats are defined exactly once. It does no I/O.
package evidence

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// chainDomain separates Logbook chain hashes from any other SHA-256 use.
const chainDomain = "logbook-evidence-v1"

// Day is one link of a device's evidence chain, as uploaded to the server.
type Day struct {
	DeviceID      string // UUID string assigned by the server
	Day           string // UTC date, YYYY-MM-DD
	PrevChainHash string // hex; "" for the device's first day
	ContentHash   string // hex SHA-256 of the canonical day encoding
	RulesVersion  string
}

var (
	hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// ErrInvalidDay is wrapped by every Day validation failure.
	ErrInvalidDay = errors.New("invalid evidence day")
	// ErrBadSignature means a signature doesn't verify.
	ErrBadSignature = errors.New("evidence signature does not verify")
)

// Validate checks the Day's field formats.
func (d Day) Validate() error {
	if _, err := time.Parse(time.DateOnly, d.Day); err != nil {
		return fmt.Errorf("%w: day %q", ErrInvalidDay, d.Day)
	}
	if len(d.DeviceID) != 36 {
		return fmt.Errorf("%w: device id", ErrInvalidDay)
	}
	if d.PrevChainHash != "" && !hexHash.MatchString(d.PrevChainHash) {
		return fmt.Errorf("%w: prev_chain_hash", ErrInvalidDay)
	}
	if !hexHash.MatchString(d.ContentHash) {
		return fmt.Errorf("%w: content_hash", ErrInvalidDay)
	}
	if d.RulesVersion == "" || len(d.RulesVersion) > 32 {
		return fmt.Errorf("%w: rules_version", ErrInvalidDay)
	}
	return nil
}

// ChainHash returns the hex chain hash for d:
// SHA-256(domain ‖ 0 ‖ device ‖ 0 ‖ day ‖ 0 ‖ prev ‖ 0 ‖ content ‖ 0 ‖ rules).
// Fields are validated first, so none can contain the 0 separator.
func ChainHash(d Day) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	h := sha256.New()
	for i, part := range []string{chainDomain, d.DeviceID, d.Day, d.PrevChainHash, d.ContentHash, d.RulesVersion} {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Sign returns the ed25519 signature over the Day's chain hash bytes.
func Sign(priv ed25519.PrivateKey, d Day) (chainHash string, sig []byte, err error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", nil, errors.New("evidence: bad private key size")
	}
	chainHash, err = ChainHash(d)
	if err != nil {
		return "", nil, err
	}
	raw, _ := hex.DecodeString(chainHash) // ChainHash always returns valid hex
	return chainHash, ed25519.Sign(priv, raw), nil
}

// Verify recomputes the chain hash and checks it matches chainHash and that
// sig is a valid signature by pub.
func Verify(pub ed25519.PublicKey, d Day, chainHash string, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: bad public key size", ErrBadSignature)
	}
	want, err := ChainHash(d)
	if err != nil {
		return err
	}
	if want != chainHash {
		return fmt.Errorf("%w: chain hash mismatch", ErrBadSignature)
	}
	raw, _ := hex.DecodeString(want) // valid hex by construction
	if !ed25519.Verify(pub, raw, sig) {
		return ErrBadSignature
	}
	return nil
}
