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
| 404 on an upload (device unknown) | forget the id (kept as `device.prev_id`), back off, register the same key again |
| 409 `public key is registered to another account` on registration | retire the key (`device.key.retired-<unix>[-n]`, kept, never overwritten), make a new one, register |
| any other 409 on registration (the active-device limit) | keep the key; show the server's message as the pause reason; retry with backoff |
| 403 with a JSON error (revoked), under an API key the server has accepted for this device | stop all crediting uploads and show "device revoked" until the API key changes; then retire the key and register a new device |
| 403 with a JSON error, under an API key that never worked with this device | the user revoked the device and already ran `logbook login`: retire the key and register a new device after backoff |

The server sends only `{"error": message}`, so the agent recognises "key taken" by its exact message, which is part of the contract (a machine-readable code would be better; see follow-ups). A 409 without a JSON body (a proxy) is retried like any transport error.

A revoked device is never re-registered silently: revoking is the user's decision on the dashboard. The agent remembers which API key last worked for the device (`device.good_key`, a fingerprint, set at registration and after each successful upload) to tell a fresh revocation from one the user has already answered. A new device starts a new chain, and minutes from before it registered are not sent under it. This also holds after `Forget`: the previous id is kept, so an account switch (404, then a 409 for the old key) still counts as a replacement and the old account's unsent minutes are never uploaded to the new one.

**Minutes.** Each run (a few seconds past every minute) computes `[now−60 min, now)` with 30 minutes of extra history. The current minute isn't computed until it ends.

- A minute is open until it is 30 minutes old: it is rewritten and re-sent whenever its content changes.
- Final minutes are only inserted if missing (offline catch-up), never rewritten. If a final minute's content differs from what the server last accepted (it changed while the server was unreachable), it is sent once more; a refusal is counted and not retried.
- If an open minute's project disappears from the result after the server saw it, it is sent as `idle`, 0 credit. All rows of one minute always go in the same request (a batch ends at a minute boundary), because the server applies a minute's decreases before its increases only within one request; that is what lets credit move between projects under the 60-second cap.
- An idle minute with no project, no credit and no flags is not stored or uploaded: it means the same as no row, and would cost the server a row per device per minute.
- Per-item rejections are logged, counted (`crediting.rejected`) and never retried unless the minute changes. A batch refused for its size (413, or the 400 "minutes per request") is split down to single minutes. Any other whole-request 400 (such as an unknown `rules_version` after an agent update the server doesn't know yet) puts minute uploads on hold for 30 minutes with the reason in status; the minutes are kept, not marked sent, and go up once the server accepts them (a minute's first write is allowed at any age).

**Evidence days.** A day is sealed once it has ended plus one hour, in order, at most 7 per run, starting at the device's registration day (and no earlier than the 90-day retention). Days with no events are skipped: a gap, which the server allows. A sealed day is stored as `pending` before upload, so retries send the same signed values. The chain advances only when the server answers 200/201. A 409 (conflict or chain mismatch) or 400 marks the day `conflict`/`rejected` and stops the chain; status shows it. The agent never re-seals a refused day or starts a second chain for the same device. A local failure while sealing (the database, the active-time source, encoding) is not a server problem: it is shown as `evidence.local_error` and retried on the next run, without backing off minute uploads. Each sealed day keeps its seal time (`evidence_days.created_at`), so a bundle can show how late a day was sealed.

**Backoff and pausing** follow the heartbeat uploader: exponential backoff with jitter (5 s to 10 min, a longer `Retry-After` wins, capped at 30 min), and on 401 a pause until the API key changes, with one retry every 30 minutes. A 403, 404 or 409 without a JSON error body (a school filter or proxy) is retried, never read as "revoked", "unknown" or "conflict". Only server answers drive backoff.

**Performance.** One run at a time. The window's events are cached in memory. The first run after a start fills the cache with a time-indexed read of the window; later runs read only rows added since, by id, so late events are still seen. On the dev VPS (`BenchmarkRunOnce`, two weeks of history plus a busy two-hour session) a warm run takes about 3 ms and a cold run about 17 ms, mostly statement preparation, against a 50 ms budget. Walking ids from zero instead took 42 ms on that database and grows with it.

**Merge dependency.** The signals module adds `ai_events.prompt_hash` (local migration 0003). When it is merged, the AI-event reads (`qAIRange`, `qAIAfter`, the scan in `readEvents`) must select it too, or the engine's repeated-prompt cut never fires on the device and evidence leaves the field out. `TestAIEventPromptHashRoundTrip` fails until they do.

## Consequences

- The evidence format is pinned by tests in `pkg/evidence`, and agent and server share one implementation.
- A step 5 bundle must contain the same rows (by the stored high-water ids), including file paths and working directories. Redacting them would change the hash; if redaction is wanted, it needs a new encoding version.
- A conflict needs a person to look at it. Logging in with a new API key starts a new device and chain.
- If the agent's clock is far ahead of the server's, rewrites of open minutes may be refused as final; they are counted, not retried.
