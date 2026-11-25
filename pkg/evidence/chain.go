package evidence

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// chainDomain separates Logbook chain hashes from any other SHA-256 use.
// ADR 0019 replaced v1 before any device sealed a day in production.
const chainDomain = "logbook-evidence-v2"

// Day is one link of a device's evidence chain, as signed and uploaded to
// the server: one block of the device's log (ADR 0017, ADR 0019).
type Day struct {
	DeviceID      string // UUID string assigned by the server
	Day           string // UTC date, YYYY-MM-DD
	PrevChainHash string // hex; "" for the device's first day
	ContentHash   string // hex Merkle root over the block's buckets
	RulesVersion  string
	// SeqFrom and SeqTo bound the block: it holds exactly the logged rows
	// with SeqFrom < seq <= SeqTo.
	SeqFrom, SeqTo int64
	// Buckets is the block's leaf count: the repo bucket plus one per
	// minute that has rows.
	Buckets int
	// TimeFrom and TimeTo are the first and last minute bucket's minute
	// (whole UTC minutes), both nil when the block has only its repo
	// bucket.
	TimeFrom, TimeTo *time.Time
}

var (
	hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// ErrInvalidDay is wrapped by every Day validation failure.
	ErrInvalidDay = errors.New("invalid evidence day")
	// ErrBadSignature means a signature doesn't verify.
	ErrBadSignature = errors.New("evidence signature does not verify")
)

// Validate checks the Day's field formats and that its block fields agree
// with each other: SeqTo > SeqFrom >= 0; Buckets >= 1; TimeFrom and
// TimeTo both nil (then Buckets is 1) or both whole minutes since 1970
// with TimeFrom <= TimeTo and room for Buckets - 1 minute buckets between
// them.
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
	if d.SeqFrom < 0 || d.SeqTo <= d.SeqFrom {
		return fmt.Errorf("%w: seq_from %d, seq_to %d", ErrInvalidDay, d.SeqFrom, d.SeqTo)
	}
	if d.Buckets < 1 {
		return fmt.Errorf("%w: buckets %d", ErrInvalidDay, d.Buckets)
	}
	switch {
	case d.TimeFrom == nil && d.TimeTo == nil:
		if d.Buckets != 1 {
			return fmt.Errorf("%w: %d buckets but no time span", ErrInvalidDay, d.Buckets)
		}
	case d.TimeFrom == nil || d.TimeTo == nil:
		return fmt.Errorf("%w: time_from and time_to must both be set or both be null", ErrInvalidDay)
	default:
		from, to := *d.TimeFrom, *d.TimeTo
		if !wholeMinute(from) || !wholeMinute(to) || from.Unix() < 0 || to.Before(from) {
			return fmt.Errorf("%w: time span %s..%s", ErrInvalidDay, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
		}
		if minutes := to.Sub(from)/time.Minute + 1; d.Buckets < 2 || int64(d.Buckets-1) > int64(minutes) {
			return fmt.Errorf("%w: %d buckets in a span of %d minutes", ErrInvalidDay, d.Buckets, minutes)
		}
	}
	return nil
}

func wholeMinute(t time.Time) bool { return t.Equal(t.Truncate(time.Minute)) }

// chainMessage is the exact byte string the chain hash covers (ADR 0019
// point 2). Every field has a fixed form, so no two Days share one:
//   - strings (the domain, device_id, day, prev_chain_hash, content_hash,
//     rules_version) are a 4-byte big-endian length, then their bytes;
//   - seq_from and seq_to are 8-byte big-endian integers;
//   - buckets is an 8-byte big-endian integer;
//   - time_from and time_to are each 0x00 when null, or 0x01 then the Unix
//     seconds as an 8-byte big-endian integer.
//
// TestChainMessageGolden pins it.
func chainMessage(d Day) []byte {
	var b []byte
	str := func(s string) {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s))) //nolint:gosec // validated fields are short
		b = append(b, s...)
	}
	u64 := func(v uint64) { b = binary.BigEndian.AppendUint64(b, v) }
	tm := func(t *time.Time) {
		if t == nil {
			b = append(b, 0x00)
			return
		}
		b = append(b, 0x01)
		u64(uint64(t.Unix())) //nolint:gosec // validated: not before 1970
	}
	str(chainDomain)
	str(d.DeviceID)
	str(d.Day)
	str(d.PrevChainHash)
	str(d.ContentHash)
	str(d.RulesVersion)
	u64(uint64(d.SeqFrom)) //nolint:gosec // validated: not negative
	u64(uint64(d.SeqTo))   //nolint:gosec // validated: not negative
	u64(uint64(d.Buckets)) //nolint:gosec // validated: positive
	tm(d.TimeFrom)
	tm(d.TimeTo)
	return b
}

// ChainHash returns the hex chain hash for d: SHA-256 of its chain message
// (see chainMessage). Fields are validated first.
func ChainHash(d Day) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(chainMessage(d))
	return hex.EncodeToString(sum[:]), nil
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

// KeyFingerprint is a short, stable identifier for a device's public key:
// the first 16 hex characters of its SHA-256. The server's device list,
// `logbook status` and `logbook verify` show it, so a person can match a
// key to a machine.
func KeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}
