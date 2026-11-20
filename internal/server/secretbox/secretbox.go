package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize is the required key length in bytes (AES-256).
const KeySize = 32

// version1 marks ciphertexts as: version (1 byte) || nonce (12) || GCM output.
const version1 byte = 1

// ErrDecrypt is returned for any ciphertext that can't be opened: wrong key,
// wrong associated data, tampering, truncation or an unknown version. The
// causes are deliberately not distinguished.
var ErrDecrypt = errors.New("secretbox: cannot decrypt")

// Box seals and opens secrets with one key. It is safe for concurrent use.
type Box struct {
	aead cipher.AEAD
}

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: creating cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: creating GCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and binds it to aad.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: reading nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, version1)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, additional(aad)), nil
}

// Open decrypts a ciphertext produced by Seal with the same aad.
func (b *Box) Open(ciphertext, aad []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(ciphertext) < 1+ns+b.aead.Overhead() || ciphertext[0] != version1 {
		return nil, ErrDecrypt
	}
	pt, err := b.aead.Open(nil, ciphertext[1:1+ns], ciphertext[1+ns:], additional(aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// additional prefixes aad with the format version so a future format can't
// be confused with this one.
func additional(aad []byte) []byte {
	out := make([]byte, 0, 1+len(aad))
	out = append(out, version1)
	return append(out, aad...)
}
