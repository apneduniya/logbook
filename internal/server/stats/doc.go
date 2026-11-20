// Package stats computes a user's dashboard aggregates (totals per project,
// language, editor, category and local day, plus streaks) from the
// user_minutes rollup, and serves them on /api/v1/me/summary and
// /api/v1/me/today.
//
// Summaries are bounded by their range (at most 366 days). Streaks need all
// of a user's history, so Service caches each user's qualifying days and,
// when told through MinutesChanged (wired after ingest commits), re-reads
// only the local days that changed.
//
// It never reads raw heartbeats, never writes, and does not own project
// settings (package projects) or public profiles (package profiles).
// Two kinds of time are reported side by side (ADR 0014):
//
//   - raw time, from the heartbeat rollup in user_minutes: minutes labelled
//     "you", the Hackatime-equivalent number in total_seconds and every
//     plain "seconds" field, with agent and idle time under labels;
//   - credited time, from the agent's labelled minutes in credited_minutes:
//     the credited_* fields, capped at 60 s per minute across a user's
//     devices, and null for users who have never registered a device.
package stats
