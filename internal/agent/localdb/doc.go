// Package localdb owns the agent's SQLite database (ADR 0009): the upload
// queue for the Logbook server, the mirror queue for Hackatime, the event
// log (ADR 0017: full heartbeats, AI events, activity samples, git events,
// Claude Code active time and the repo mapping, each row numbered from one
// counter) with consistent snapshot reads, a short heartbeat log used for
// local estimates and repo discovery, the struggle engine's credited
// minutes with their computation keys, finality and upload state, the
// device's evidence chain (ADR 0010, ADR 0019: each sealed block's record
// and bucket list, the block being sealed with its frozen bound and hasher
// state, and where each device's chain starts), and a small key-value
// table for agent state.
//
// It uses modernc.org/sqlite (pure Go, no cgo) in WAL mode with a busy
// timeout, applies embedded, versioned, forward-only migrations on Open, and
// keeps tables bounded: queues are capped by row count (oldest rows are
// dropped and counted); logged rows are kept for 90 days and capped by row
// count, but only rows the evidence chain has sealed are ever deleted (a
// full table of unsealed activity samples drops new samples, counted);
// credited minutes are kept for 90 days, the heartbeat log for two days,
// and evidence days and their bucket lists (about 100 KB for a busy day)
// for as long as the chain exists.
//
// It does not decide what to upload or when (internal/agent/upload,
// internal/agent/crediting), and it never sends anything off the device.
package localdb
