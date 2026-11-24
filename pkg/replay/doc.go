// Package replay turns a device's logged rows (evidence.Row) into the
// struggle engine's input for one computation key (T, H) (ADR 0017, ADR
// 0019), and says what a replay compares. The agent's live runs, the
// agent's own replays and the server's re-check of submissions (ADR 0020)
// all build the engine's input here, so they can't drift.
//
// It does no I/O. It doesn't read the log, check proofs (pkg/evidence),
// choose which minutes to replay or store results.
package replay
