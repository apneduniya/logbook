// Package credited accepts the agent's labelled minutes on
// POST /api/v1/devices/{id}/minutes (ADR 0010) and stores them in
// credited_minutes. It does not compute credit (the agent runs pkg/struggle),
// read stats (package stats sums credited_minutes) or own devices (package
// devices).
//
// # Rules for one batch
//
// The whole request is rejected (400) when it is malformed, has more than
// MaxBatch items, or names an unknown rules version. The device must belong
// to the caller (404 otherwise) and must not be revoked (403). Each item is
// then accepted or rejected on its own, by Plan, which is pure:
//
//   - the minute is RFC 3339 in UTC on a whole minute, at most 2 minutes
//     ahead of the server clock; the project is at most 256 bytes with no
//     control characters; the label is you, agent or idle; credited is
//     0..60 and 0 unless the label is you; cuts and flags are short codes,
//     and multi_device is never accepted (only the server sets it);
//   - a (minute, project) may be written for the first time whenever the
//     agent catches up, but an existing row may only be changed while the
//     minute is less than FinalAfter (30 minutes) old by the server clock.
//     Sending exactly the stored values again is always accepted;
//   - across all projects of one minute of one device, credited seconds
//     never exceed 60. Items that lower a minute's credit are applied first,
//     then the rest in request order, so moving a minute from one project
//     to another works in one batch; an item that would push the minute
//     over 60 is rejected and later items are still tried;
//   - the same (minute, project) twice in one batch: the later one is
//     rejected.
//
// # Concurrency
//
// The upload runs in one transaction that first locks the device row (FOR
// NO KEY UPDATE), so concurrent batches for a device are serialised and the
// 60-second rule is checked against committed data; a concurrent revoke
// waits for the upload to finish. Accepted rows are written in one
// statement.
package credited
