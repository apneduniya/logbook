// Package ledger stores each device's signed evidence chain (ADR 0010): one
// record per device per UTC day, uploaded by the agent on
// POST /api/v1/devices/{id}/evidence-days. The chain hash and signature
// formats belong to pkg/evidence; this package only checks uploads against
// them and against the chain already stored.
//
// It does not own devices (package devices), credited minutes (package
// credited) or submissions, which later compare the chain with a bundle of
// raw events (step 5).
//
// # Rules
//
// For a device that belongs to the caller (404 otherwise) and isn't revoked
// (403), in one transaction that locks the device row:
//
//   - the fields must be well formed and the day not after today (UTC), or
//     the request is 400;
//   - chain_hash must equal evidence.ChainHash of the fields, and the
//     signature must verify with the device's registered public key, or 400;
//   - an identical record for the same day is accepted again with 200;
//   - a different, validly signed record for a stored day is 409 and is
//     written to the audit log as evidence_conflict: the device's key signed
//     two different histories for one day;
//   - prev_chain_hash must equal the chain_hash of the device's latest stored
//     day before this one, or "" when there is none, and a stored later day
//     must not already point past this one; otherwise 409 "chain mismatch".
//     Gaps (days with no record) are allowed.
//
// A new record is 201.
package ledger
