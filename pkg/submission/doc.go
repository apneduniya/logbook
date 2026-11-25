// Package submission holds what a submission of hours (ADR 0020) means,
// in one copy shared by the agent (logbook submit), the server (upload,
// re-check job, review packet) and the offline checker (logbook verify):
//
//   - the wire types: the plan the server answers the agent with (Plan),
//     the upload and its evidence part (Upload, Part, Block, PartMinute),
//     and the verification bundle the admin API exports (Bundle, ADR 0022);
//   - the verifier: given a part, the device's signed evidence records and
//     public key, and the device's frozen minute rows, the result of each
//     row, verified, mismatch or unbacked (VerifyPart), following ADR 0019
//     and docs/api/submissions.md "The re-check job";
//   - the multi-device cap (ADR 0014): the capped seconds per minute
//     recomputed from the frozen cap inputs exactly as the
//     credited_project_minutes view computes them (Cap, CheckCap);
//   - the review packet built from the results (BuildPacket), and the
//     Airtable justification text built from a packet and its link when
//     the packet is served (Justification);
//   - the whole offline check of a bundle (CheckBundle).
//
// It does no I/O and imports nothing from internal/. It doesn't store
// submissions, decide who may submit, mask trust levels or count raw
// heartbeat time: callers pass those in.
package submission
