// Package hackatime links a Logbook account to the user's Hackatime account
// and carries over what Hackatime knows about them (ADR 0004):
//
//   - Client talks to Hackatime's HTTP API and OAuth (Doorkeeper) endpoints
//     with timeouts, a process-wide request budget (Limiter), bounded
//     responses, retries on 429/5xx and strict decoding.
//   - Service runs the link flow (OAuth state + PKCE, encrypted token
//     storage, one Hackatime account per Logbook user), unlink, link status
//     and the trust level used to gate submissions.
//   - Importer copies the user's Hackatime heartbeat history into Logbook
//     through a HeartbeatSink: the last 30 days first, then older history,
//     in windows of whole days sized by a cheap activity probe, a few at a
//     time, persisting progress as windows finish.
//   - TrustRefresher re-reads trust levels on a schedule.
//   - Handler exposes the browser and JSON endpoints; workers.go adapts the
//     importer and refresher to River jobs.
//
// It does not store heartbeats itself (that is ingest's job), does not
// authenticate Logbook users (the session middleware is injected), and never
// holds Hackatime write credentials (ADR 0005).
package hackatime
