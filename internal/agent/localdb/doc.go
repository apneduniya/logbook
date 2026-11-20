// Package localdb owns the agent's SQLite database (ADR 0009): the upload
// queue for the Logbook server, the mirror queue for Hackatime, the raw local
// evidence (AI events, activity samples, git events), a short heartbeat log
// used for local estimates and repo discovery, and a small key-value table
// for agent state.
//
// It uses modernc.org/sqlite (pure Go, no cgo) in WAL mode with a busy
// timeout, applies embedded, versioned, forward-only migrations on Open, and
// keeps every table bounded: queues are capped by row count (oldest rows are
// dropped and counted), raw events are kept for 90 days, the heartbeat log
// for two days.
//
// It does not decide what to upload or when (internal/agent/upload), and it
// never sends anything off the device.
package localdb
