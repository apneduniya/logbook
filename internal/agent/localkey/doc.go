// Package localkey derives the device-local secrets the agent needs from the
// local token in config.json (config.Config.LocalToken), so no second secret
// has to be generated, stored or rotated:
//
//   - PromptKey keys the prompt hashes hook commands record (a keyed hash of
//     a prompt can't be checked against a guess without the key);
//   - OTLPToken authenticates Claude Code's OpenTelemetry exporter to the
//     daemon's /v1/metrics endpoint. It is written into
//     ~/.claude/settings.json, which people sometimes publish with their
//     dotfiles, so it is a one-way derivation: leaking it reveals neither the
//     local token nor the prompt key.
//
// Both use HKDF-SHA256 (RFC 5869) with the local token's bytes as the input
// keying material, no salt, and a fixed, versioned info string per use. The
// package is pure: no I/O, no clock.
package localkey
