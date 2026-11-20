// Package secretbox encrypts small secrets (third-party OAuth tokens) for
// storage at rest with AES-256-GCM. Each ciphertext carries a version byte
// and a random nonce, and is bound to associated data (normally the owning
// user's id) so a ciphertext copied to another row fails to decrypt.
//
// It owns the ciphertext format only. It does not load or rotate keys (that
// is config's job) and does no I/O.
package secretbox
