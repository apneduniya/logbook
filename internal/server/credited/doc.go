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
//     ahead of the server clock; the project is normalised like a
//     heartbeat project (pkg/projectname) and then at most 256 bytes; the
//     label is you, agent or idle; credited is 0..60 and 0 unless the label
//     is you; cuts and flags are short codes, and multi_device is never
//     accepted (only the server sets it);
//   - sending exactly the stored values again is always accepted;
//   - the minute is on or after the UTC day the device was registered, and
//     not in a UTC day the device has sealed with an evidence record, so a
//     new key can't write history for closed days;
//   - a minute is final once it is FinalAfter (30 minutes) old by the
//     server clock. A stored (minute, project) may only change while the
//     minute is open, and a final minute takes no new projects once the
//     device has stored any row for it, so idle or agent time can't become
//     "you" time later under another project. A minute never sent may be
//     sent at any time, so an offline agent can catch up; if its catch-up
//     splits a minute across batches, more projects are accepted for
//     CatchUpWindow after the minute's first upload, but only when every
//     row of it was first uploaded after it was final;
//   - one device stores at most MaxProjectsPerMinute projects per minute,
//     which bounds rows per device-minute whatever their credit. The engine
//     (pkg/struggle) never emits more, so this only stops other clients;
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
