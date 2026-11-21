// Package crediting runs the struggle engine inside the agent daemon and
// uploads its results (docs/STRUGGLE-RULES.md, ADR 0010, ADR 0013, ADR 0017).
//
// Every minute a Runner opens one snapshot of the local event log, reads
// its high-water mark H, the last two hours of events (a 60-minute window
// plus 60 minutes of history) and the repo mapping at H, and calls
// struggle.Compute once. The run's computation key is (T, H), T being the
// end of its window: a run is a pure function of the log and its key, and
// every stored minute records the key that produced it. A minute's rows
// are rewritten, under the new key, only when the result changes; final
// minutes are never recomputed. A minute becomes final at the first
// settled run with T >= minute + FinalLead; after a start or a wake the
// agent settles first, so late honest events still count. Rows the server
// hasn't answered for go to POST /api/v1/devices/{id}/minutes with their
// keys; when the server refuses a minute as final it returns the rows it
// holds, and the agent adopts them. After each UTC day ends, plus an hour's
// grace, the agent seals the log rows logged since its previous block: the
// canonical content hash, chained to the previous day and signed with the
// device key, uploaded to POST /api/v1/devices/{id}/evidence-days.
// ReplayMinute and ReplayBlock reproduce a minute at its key and a block's
// rows from the log.
//
// It owns: the window arithmetic, converting logged events into the
// engine's Input, which minutes are kept, final and uploaded, the evidence
// chain's progress, and backoff and pausing for these uploads (the same
// rules as the heartbeat uploader). It does not own the engine
// (pkg/struggle), the encoding and signature formats (pkg/evidence), the
// device key and registration (internal/agent/devicekey), the HTTP calls
// (internal/agent/serverapi) or storage (internal/agent/localdb). Raw events
// never leave the device here: only labelled minutes with their keys and
// signed hashes are uploaded.
package crediting
