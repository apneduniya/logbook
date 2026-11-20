// Package upload drains the agent's two heartbeat queues: one Worker sends
// the Logbook queue to the Logbook server, another mirrors the Hackatime
// queue to Hackatime (ADR 0005). Both use wakatime-cli's bulk endpoint shape,
// batches of at most 100, exponential backoff with jitter, and per-item
// results.
//
// It owns: when to send, how to read per-item results, backoff, pausing on
// authentication failures, and persisting each worker's last success and
// error so `logbook status` can show them even when the daemon is down. It
// does not own the queues' storage (internal/agent/localdb) or the
// credentials, which it reads through a Target function on every attempt so
// a fresh `logbook login` takes effect without a restart.
package upload
