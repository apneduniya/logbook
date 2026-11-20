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
// Credited time is minutes labelled "you"; agent and idle time are reported
// alongside it but not added to totals.
package stats
