// Package ingest accepts WakaTime-format heartbeats (ADR 0005), stores them
// deduplicated, and keeps the per-minute rollups in user_minutes up to date.
// It also serves the WakaTime status-bar endpoint, which reads those rollups.
//
// It owns: the heartbeat endpoints and their wakatime-cli contract, heartbeat
// normalisation and validation, the insert-and-recompute transaction, and the
// "today" status bar. It does not own authentication (the API-key middleware
// is injected), dashboards or program stats (internal/server/stats), live
// fan-out (it only calls a Publisher), or the duration rule itself, which
// lives in the pure package ingest/rollup (see its doc for the rule, the
// 60-second cap and how empty projects are named).
//
// # One batch, one transaction
//
// Service.Ingest validates each heartbeat, then in a single transaction:
//
//  1. takes a per-user advisory lock (pg_advisory_xact_lock), so concurrent
//     batches for one user serialise and never recompute the same minutes
//     from different snapshots;
//  2. inserts the batch in one statement (ON CONFLICT (user_id, fields_hash,
//     time) DO NOTHING) that also creates projects rows for new names;
//  3. reads every heartbeat in the affected windows in one query;
//  4. replaces the 'you' rows of user_minutes in those windows in one
//     statement.
//
// After commit it calls Publisher.MinutesChanged with the span of the
// recomputed windows. A batch that inserts nothing (all duplicates or all
// invalid) changes no rollups and publishes nothing.
//
// # Heartbeat fields
//
// The HTTP layer fills fields that wakatime-cli sends in headers, matching
// Hackatime: editor, operating system and plugin come from the heartbeat's
// user_agent (or the User-Agent header when absent) and win over the body;
// machine comes from X-Machine-Name and wins over the body. Every string has
// NUL bytes removed (Postgres can't store them), project names have control
// characters stripped and are trimmed, empty category becomes "coding" and
// empty type becomes "file".
package ingest
