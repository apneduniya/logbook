package aievent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/apneduniya/logbook/pkg/wire"
)

// Prompt decodes a JSON prompt string into what Logbook keeps of it: its
// length in runes and the SHA-256 digest of its normalised form. The text
// itself is dropped inside UnmarshalJSON and never stored in any field, so a
// Prompt can't leak it through logging or marshalling.
//
// A non-negative integer decodes as an already-counted length with no digest
// (Salvage writes one for a prompt cut off by the payload cap; a cut-off
// prompt can't be hashed). Anything else decodes as an empty Prompt.
type Prompt struct {
	runes  int
	digest PromptDigest
}

// PromptDigest is SHA-256(NormalizePrompt(prompt)). It is an intermediate
// value: unkeyed, so it could be checked against guesses, and it never leaves
// the hook process. Only PromptHash, keyed with the device's prompt key, is
// recorded.
type PromptDigest struct {
	sum [sha256.Size]byte
	ok  bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (p *Prompt) UnmarshalJSON(b []byte) error {
	*p = Prompt{}
	var s string
	if len(b) > 0 && b[0] == '"' && json.Unmarshal(b, &s) == nil {
		*p = Prompt{runes: Runes(s), digest: DigestPrompt(s)}
		return nil
	}
	var n int
	if json.Unmarshal(b, &n) == nil && n > 0 {
		p.runes = n
	}
	return nil
}

// MarshalJSON writes only the length, so a Prompt that ends up in a log or
// debug dump carries nothing private.
func (p Prompt) MarshalJSON() ([]byte, error) { return json.Marshal(p.runes) }

// Len is the prompt's length in runes.
func (p Prompt) Len() int { return p.runes }

// Digest is the prompt's digest; it is unset for a prompt that wasn't a
// complete JSON string.
func (p Prompt) Digest() PromptDigest { return p.digest }

// NormalizePrompt is the form a prompt is hashed in, so that re-sending the
// same request with different spacing or capitalisation counts as the same
// prompt: invalid UTF-8 is dropped, leading and trailing white space removed,
// every run of white space (unicode.IsSpace) collapsed to one ASCII space,
// and the result lowercased rune by rune (unicode.ToLower, Go's
// strings.ToLower). There is no Unicode NFC step: it would need
// golang.org/x/text tables in every hook process, and the loop rule compares
// prompts within one session of one tool, where the same request arrives in
// the same encoding.
func NormalizePrompt(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " "))
}

// DigestPrompt returns SHA-256(NormalizePrompt(s)).
func DigestPrompt(s string) PromptDigest {
	return PromptDigest{sum: sha256.Sum256([]byte(NormalizePrompt(s))), ok: true}
}

// ParsePromptDigest reads a hex SHA-256 digest computed elsewhere (the
// OpenCode plugin digests prompts itself so their text never leaves it). A
// malformed value gives an unset digest.
func ParsePromptDigest(h string) PromptDigest {
	var d PromptDigest
	if len(h) != hex.EncodedLen(sha256.Size) {
		return d
	}
	if _, err := hex.Decode(d.sum[:], []byte(h)); err != nil {
		return PromptDigest{}
	}
	d.ok = true
	return d
}

// Hex returns the digest hex-encoded, or "" when unset. It is the format the
// OpenCode plugin sends (ParsePromptDigest reads it back).
func (d PromptDigest) Hex() string {
	if !d.ok {
		return ""
	}
	return hex.EncodeToString(d.sum[:])
}

// IsSet reports whether the digest holds a value.
func (d PromptDigest) IsSet() bool { return d.ok }

// Keyed returns hex(HMAC-SHA256(key, digest)), the wire.AIEvent.PromptHash
// value, or "" when the digest or the key is missing.
func (d PromptDigest) Keyed(key []byte) string {
	if !d.ok || len(key) == 0 {
		return ""
	}
	m := hmac.New(sha256.New, key)
	m.Write(d.sum[:]) // hash.Hash.Write never returns an error
	return hex.EncodeToString(m.Sum(nil))
}

// PromptHash returns the PromptHash for an event built from f: the keyed
// digest for a prompt_submit with a digest, else "".
func PromptHash(f Fields, key []byte) string {
	if f.Kind != wire.AIPromptSubmit {
		return ""
	}
	return f.PromptDigest.Keyed(key)
}
