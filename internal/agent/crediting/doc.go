// Package crediting runs the struggle engine inside the agent daemon and
// uploads its results (docs/STRUGGLE-RULES.md, ADR 0010, ADR 0013).
//
// Every minute a Runner reads the last two hours of local events (a
// 60-minute window plus 60 minutes of history), calls struggle.Compute with
// the events that arrived by the run's cutoff, stores the labelled minutes
// in the local database, and uploads the ones the server doesn't have yet
// to POST /api/v1/devices/{id}/minutes. Minutes are rewritten and re-sent
// when they change until they are final: normally 29 minutes after they
// start, and after a start or a sleep only once the agent has settled
// (ADR 0016); once final they are sent at most once more. After each UTC day
// ends, plus an hour's grace, it seals the day: the canonical content hash of
// the day's raw events, chained to the previous day and signed with the
// device key, uploaded to POST /api/v1/devices/{id}/evidence-days.
//
// It owns: the window arithmetic, converting local events into the engine's
// Input, which minutes are kept and uploaded, the evidence chain's progress,
// and backoff and pausing for these uploads (the same rules as the heartbeat
// uploader). It does not own the engine (pkg/struggle), the encoding and
// signature formats (pkg/evidence), the device key and registration
// (internal/agent/devicekey), the HTTP calls (internal/agent/serverapi) or
// storage (internal/agent/localdb). Raw events never leave the device here:
// only labelled minutes and signed hashes are uploaded.
package crediting
