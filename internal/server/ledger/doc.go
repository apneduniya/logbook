// Package ledger stores each device's signed evidence chain (ADR 0010, ADR
// 0019): one record per device per UTC day, uploaded by the agent on
// POST /api/v1/devices/{id}/evidence-days, and reads it back for the
// submission re-check (Service.Days). The chain hash and signature formats
// belong to pkg/evidence; this package only checks uploads against them and
// against the chain already stored.
//
// It does not own devices (package devices), credited minutes (package
// credited) or submissions, which check proofs against the stored records
// (ADR 0020).
//
// # Rules
//
// For a device that belongs to the caller (404 otherwise) and isn't revoked
// (403), in one transaction that locks the device row:
//
//   - a body without seq_from, seq_to or buckets comes from an agent older
//     than ADR 0019 and is 400 "update the Logbook agent";
//   - the fields must be well formed (evidence.Day.Validate, time_from and
//     time_to in UTC), the rules version one the server knows, and the day
//     not after today (UTC) and not before the UTC day the device was
//     registered, or the request is 400;
//   - chain_hash must equal evidence.ChainHash of the fields, and the
//     signature must verify with the device's registered public key, or 400;
//   - an identical record for the same day is accepted again with 200;
//   - a different, validly signed record for a stored day is 409 and is
//     written to the audit log as evidence_conflict: the device's key signed
//     two different histories for one day;
//   - prev_chain_hash must equal the chain_hash of the device's latest stored
//     day before this one, or "" when there is none, and a stored later day
//     must not already point past this one; otherwise 409 "chain mismatch".
//     Gaps (days with no record) are allowed;
//   - seq_from must equal the seq_to of the device's latest stored day before
//     this one (and seq_to the seq_from of a stored later day), or 409. The
//     device's first stored day may start at any seq_from: 0 on a fresh
//     install, or the log's high-water mark at a takeover (ADR 0019 point 4).
//
// A new record is 201.
package ledger
