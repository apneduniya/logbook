// Package rollup turns a user's heartbeats into per-minute rollup rows. It is
// pure: plain values in, plain values out, no I/O and no clock. It owns the
// duration rule and the recompute-window arithmetic; reading heartbeats and
// writing user_minutes belong to the ingest package.
//
// # Duration rule
//
// A user's heartbeats form one timeline ordered by (time, fields_hash),
// across all of their machines and editors. Each heartbeat is credited the
// gap to the next heartbeat on that timeline, attributed to the earlier
// heartbeat's project, language, editor and category:
//
//   - gap <= Timeout (2 minutes): the whole gap is credited;
//   - gap > Timeout, or no next heartbeat: nothing is credited.
//
// This is WakaTime's "keystroke timeout" rule. Hackatime differs slightly: it
// credits min(gap, 2 minutes) even when the gap is longer, which adds up to
// two minutes per break. We follow WakaTime because a long gap means the
// person was not coding.
//
// A credited span [t, t+gap) is split across the minute boundaries it covers.
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
// changes only its own credit, which lies in [t, t+Timeout], and its
// predecessor's credit, which lies in [t-Timeout, t). Windows therefore
// recomputes the whole minutes covering [t-Timeout, t+Timeout] for every new
// heartbeat, merging windows that are close together. Recomputing a window
// needs the heartbeats in ReadRange(window), which adds Timeout on each side
// so the neighbours just outside the window are included. Recomputing every
// affected window from the stored heartbeats gives the same rows whatever
// order the heartbeats arrived in.
package rollup
