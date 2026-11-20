// Package devices registers agent devices and lets users list and revoke
// them (ADR 0010). A device is one agent install with its own ed25519 key;
// the server stores only the public key and checks evidence signatures with
// it.
//
// It owns POST /api/v1/devices (API-key auth, called by the agent) and
// GET/DELETE /api/v1/me/devices (session auth, called by the web UI), and
// the device.register and device.revoke audit entries. It does not own
// authentication (the middleware is injected), credited-minute uploads
// (package credited) or evidence days (package ledger).
//
// # Registration
//
// Registration is idempotent per public key: the same user registering the
// same key again gets the existing id back with 200, whatever name and OS it
// sends; a key already registered to another user is 409. A revoked device
// is never revived: registering its key again returns its id, and uploads
// with it keep getting 403, so the agent must generate a new key. A user can
// have at most MaxActiveDevices unrevoked devices.
//
// Revoking stops future uploads and keeps the device's history, so credited
// minutes and evidence uploaded before stay auditable.
package devices
