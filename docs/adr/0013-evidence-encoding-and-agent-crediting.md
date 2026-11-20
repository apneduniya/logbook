# 0013: Canonical evidence encoding, device identity and the agent's crediting pipeline

- Status: accepted
- Date: 2026-11-20

## Context

ADR 0010 fixes what the agent uploads (labelled minutes, one signed hash per UTC day) and the chain hash format. It leaves open:

- the exact bytes a day's content hash covers;
- how the agent stores the device key and what it does when the server revokes or forgets the device;
- how late events, restarts and server refusals interact with "final" minutes and the chain;
- where the heartbeats come from, since the upload queue deletes them once sent.

## Decision

**Canonical encoding** (`pkg/evidence.Encode`, `ContentHash`).

- JSON Lines, one `{"t", "type", "v"}` object per event, `\n`-terminated, sorted by time, then type, then the bytes of `v`.
- `t` is RFC 3339 UTC with nanoseconds; `type` is `activity`, `active_time`, `ai`, `git` or `heartbeat`.
- `v` is the `pkg/wire` struct (or `evidence.ActiveTime`) encoded by `encoding/json` without HTML escaping, with every `time.Time` in UTC. **The Go field order of those structs is part of the format.** A golden file and a pinned hash fail if it changes; changing it needs a new chain domain.
- An event outside the day is an error, not silently dropped.
- Only the agent's own reading of each event is covered: times as stored locally (milliseconds for sensor events, heartbeats exactly as received).

**Which events a day covers.** When a day is sealed the agent records the highest row id of each local event table. The day's content is exactly its events at or below those ids. Late events (an editor's offline queue, the hook spool) never change a sealed day, and a step 5 bundle can reproduce the hash from the same rows. Active-time datapoints come from an optional source without row ids; they are read at sealing time, an hour after the day ends.

**Full heartbeats are kept locally** in a new `heartbeats` table (90 days, deduplicated by `wire.FieldsHash`). They hold file paths, which the server already receives in heartbeats; nothing new leaves the device.

**Device key.** ed25519, stored as a PKCS#8 PEM `PRIVATE KEY` in `<data dir>/device.key`, mode 0600 (on Windows the data directory's ACL). PEM over a raw seed because standard tools can read it and the format names its algorithm. Only the single-instance daemon creates it. A damaged key file is an error, never silently replaced.

**Device lifecycle.**

| Server says | Agent does |
|---|---|
| device registered (201/200) | store id, server URL and time in `kv` |
| server URL changed | register the same key with the new server |
| 404 on an upload (device unknown) | forget the id, back off, register the same key again |
| 409 on registration (key belongs to another account) | retire the key (`device.key.retired-<unix>`, kept, not deleted), make a new one, register |
| 403 with a JSON error (revoked) | stop all crediting uploads and show "device revoked" until the API key changes; then retire the key and register a new device |

A revoked device is never re-registered silently: revoking is the user's decision on the dashboard. A new device starts a new chain, and minutes from before it registered are not sent under it.

**Minutes.** Each run (a few seconds past every minute) computes `[now−60 min, now)` with 30 minutes of extra history. The current minute isn't computed until it ends.

- A minute is open until it is 30 minutes old: it is rewritten and re-sent whenever its content changes.
- Final minutes are only inserted if missing (offline catch-up), never rewritten. If a final minute's content differs from what the server last accepted (it changed while the server was unreachable), it is sent once more; a refusal is counted and not retried.
- If an open minute's project disappears from the result after the server saw it, it is sent as `idle`, 0 credit. Within a minute, rows losing credit are sent before rows gaining it, so the server's 60-second cap holds.
- An idle minute with no project, no credit and no flags is not stored or uploaded: it means the same as no row, and would cost the server a row per device per minute.
- Per-item rejections are logged, counted (`crediting.rejected`) and never retried unless the minute changes. A batch refused as a whole (400/413 with a JSON error) is split down to single minutes.

**Evidence days.** A day is sealed once it has ended plus one hour, in order, at most 7 per run, starting at the device's registration day (and no earlier than the 90-day retention). Days with no events are skipped: a gap, which the server allows. A sealed day is stored as `pending` before upload, so retries send the same signed values. The chain advances only when the server answers 200/201. A 409 (conflict or chain mismatch) or 400 marks the day `conflict`/`rejected` and stops the chain; status shows it. The agent never re-seals a refused day or starts a second chain for the same device.

**Backoff and pausing** follow the heartbeat uploader: exponential backoff with jitter (5 s to 10 min, a longer `Retry-After` wins, capped at 30 min), and on 401 a pause until the API key changes, with one retry every 30 minutes. A 403 or 404 without a JSON error body (a school filter or proxy) is retried, never read as "revoked".

**Performance.** One run at a time. The window's events are cached in memory and only rows added since the last run are read, by id, so late events are still seen. A warm run takes about 2–3 ms on the dev VPS for a busy two-hour session, and a cold run (the first after start) about 10 ms, against a 50 ms budget.

## Consequences

- The evidence format is pinned by tests in `pkg/evidence`, and agent and server share one implementation.
- A step 5 bundle must contain the same rows (by the stored high-water ids), including file paths and working directories. Redacting them would change the hash; if redaction is wanted, it needs a new encoding version.
- A conflict needs a person to look at it. Logging in with a new API key starts a new device and chain.
- If the agent's clock is far ahead of the server's, rewrites of open minutes may be refused as final; they are counted, not retried.
