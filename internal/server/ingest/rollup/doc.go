// Package rollup turns a user's heartbeats into per-minute rollup rows. It is
// pure: plain values in, plain values out, no I/O and no clock. It owns the
// duration rule and the recompute-window arithmetic; reading heartbeats and
// writing user_minutes belong to the ingest package.
//
// # Duration rule
//
// A user's heartbeats form one timeline ordered by (time, fields_hash),
// across all of their machines and editors. Each heartbeat is credited
// min(gap since the previous heartbeat, Timeout), attributed to that
// heartbeat's own project, language, editor and category, and placed in the
// span [t-credit, t) just before it. The user's first heartbeat is credited 0.
//
// This is exactly Hackatime's rule (LEAST(time - LAG(time), 2 minutes) over
// the user's heartbeats), so raw totals match what programs read from
// Hackatime today. It deliberately credits up to two minutes per break;
// deciding what is real working time is the struggle engine's job
// (credited_minutes, docs/STRUGGLE-RULES.md), not the raw rollup's.
//
// A credited span is split across the minute boundaries it covers.
//
// # 60-second cap
//
// Because every span ends where the next one starts, spans never overlap and
// a minute can never hold more than 60 seconds, even when two machines send
// heartbeats at the same time: they share one timeline, so concurrent
// machines split the wall clock instead of doubling it. The cap is still
// enforced defensively: if a minute's groups ever add up to more than 60
// seconds, every group in it is scaled down proportionally. Seconds are then
// rounded to whole numbers with the largest-remainder method, so the rounded
// groups of a minute add up to the rounded total and never exceed 60.
//
// # Empty project
//
// Heartbeats with no project are rolled up under the project name "Unknown",
// which is how WakaTime and Hackatime display them. Raw heartbeats keep the
// empty string, and no projects row is created for it.
//
// # Recompute windows
//
// Heartbeats arrive out of order (bulk uploads, offline queues, imports), so a
// batch can change minutes that were already rolled up. A new heartbeat at t
// changes only its own credit, which lies in [t-Timeout, t), and its
// successor's credit, which lies in [t, t+Timeout). Windows therefore
// recomputes the whole minutes covering [t-Timeout, t+Timeout] for every new
// heartbeat, merging windows that are close together. Recomputing a window
// needs the heartbeats in ReadRange(window), which adds Timeout on each side,
// plus the latest heartbeat before that range so every gap is known. Recomputing every
// affected window from the stored heartbeats gives the same rows whatever
// order the heartbeats arrived in.
package rollup
