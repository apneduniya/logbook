// Package devicekey owns the device's identity (ADR 0010, ADR 0013): its
// ed25519 signing key on disk and its registration with the Logbook server.
//
// The key is stored at <data dir>/device.key as a PKCS#8 PEM "PRIVATE KEY"
// block, mode 0600 (on Windows the data directory's ACL protects it, as for
// config.json). Only the daemon creates it; the daemon is single-instance.
// The private key never leaves the Key type: it isn't exported, and the
// type's String, GoString and slog LogValue print a placeholder.
//
// Identity registers the public key (POST /api/v1/devices) and keeps the
// device id, the server it belongs to and the registration time in the
// local key-value table. It re-registers when the server URL changes or the
// server forgets the device, and moves to a fresh key when the key is
// registered to another account. A device the server revoked stays revoked
// until the API key changes (`logbook login`); then the old key is retired
// and a new device is registered.
//
// It does not decide when to register or what to sign
// (internal/agent/crediting), and it never logs or uploads the private key.
package devicekey
