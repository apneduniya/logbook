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

**Which events a day covers.** One rule for every event type: a sealed day's content is fixed at sealing time, and anything that arrives or changes later is not part of it. How each type is pinned:

| Type | Pinned by | Why that is enough |
|---|---|---|
| heartbeats, AI events, activity samples, git events | the highest row id of each table at sealing time (`evidence_days.hw_*`); the day's content is its rows at or below those ids | the tables are append-only (rows are inserted, never updated; a duplicate heartbeat or commit is ignored, not rewritten) and each row holds its final values, including the AI event's project, which is derived once at ingest |
| active time | the exact records hashed, stored with the day (`evidence_days.active_time`, migration 0004) | points arrive late (OTLP export interval, delta temporality, exporter retries), and their derived fields (the session's working directory from hook events, which sessions the receiver's uptime covered) are read from other tables that keep changing, so no id range can reproduce them |

`crediting.ReplayDay` rebuilds a sealed day from exactly this (rows up to the marks plus the stored active time); sealing hashes what the same function returns, so the two can't drift. Late events (an editor's offline queue, wakatime-cli's offline queue, the hook spool, a delayed OTLP export) never change a sealed day. If local data no longer holds the day as sealed (raw events pruned after 90 days or trimmed by the row cap, or a day sealed before migration 0004 that had active time), `ReplayDay` returns `ErrNotReproducible`, checked against the stored event counts, rather than a different hash. The stored active time is cleared with the day's raw events after the retention period.

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
- A final minute is frozen whole: its rows are never rewritten and it takes no new project. Only a final minute with no rows at all is filled in (offline catch-up). Otherwise a late event or a changed repo mapping could add a second project row to a final minute, crediting it twice locally in a way the server refuses and a replay can't reproduce. If a final minute's content differs from what the server last accepted (it changed while the server was unreachable), it is sent once more; a refusal is counted and not retried.
- **Repo mapping.** The engine maps working directories and file paths to projects by the active repos (`Input.Repos`), which the daemon finds from the filesystem: not reproducible, and it changes (a project renamed, a repo first named by a heartbeat). Each run records the active repos in an append-only local journal (`repo_map`, migration 0005: a row when a path is first seen or its project changes, never updated). Minute M is computed with each path's latest row recorded before M became final (`since < M + 30 min`, `crediting.ReposAt`); a run whose window spans a mapping change runs the engine once per group of minutes sharing a mapping and keeps each group's own minutes. Every one of those engine calls gets the same input except `Repos`: the whole window, the same events and active time, and `DataEnd` at the window's end, because the engine reads the window's bounds (a turn quiet for 10 minutes before the end of the data has ended; `long_day` and `otel_mismatch` total the window). When a change moves no event to another project, the grouped result equals one call over the window (`TestGroupedComputationMatchesFullRun`). So a minute's project doesn't depend on when it is recomputed, a change never reaches a final minute, and a replay rebuilds the mapping of any minute from the journal rows. A row is pruned only once a newer row for its path is older than the 90-day retention. If the repo source fails, the last recorded mapping stays in force. The mapping is not part of the day's content hash (the encoding is pinned); a step 5 bundle carries the journal rows recorded before the day's end plus 30 minutes alongside the day's events.
- If an open minute's project disappears from the result after the server saw it (it was displaced from the top 8, or its events moved), it is sent as `idle` with no credit, cuts or flags, which the server reads as a removal and deletes (ADR 0014); it sorts first in its minute. All rows of one minute always go in the same request (a batch ends at a minute boundary), because the server applies a minute's removals, then its decreases, then its increases only within one request; that is what lets a project replace another under the 8-project limit and credit move between projects under the 60-second cap.
- An idle minute with no project, no credit and no flags is not stored or uploaded: it means the same as no row, and would cost the server a row per device per minute.
- Per-item rejections are logged, counted (`crediting.rejected`) and never retried unless the minute changes. A batch refused for its size (413, or the 400 "minutes per request") is split down to single minutes. Any other whole-request 400 (such as an unknown `rules_version` after an agent update the server doesn't know yet) puts minute uploads on hold for 30 minutes with the reason in status; the minutes are kept, not marked sent, and go up once the server accepts them (a minute's first write is allowed at any age).

**Evidence days seal their minutes.** The server refuses first-time minutes in a day the device has sealed, so a day must reach the server only after every one of its minutes. The agent seals a new day only on a run whose minute uploads went through (not on hold after a refused batch, not backing off, not paused), and only when no stored minute up to the day's end is still waiting for the server's answer. One run uploads at most 20 batches; when that budget runs out with minutes left (budget exhausted, not drained), the day waits for a later run. The engine never writes into a sealable day: its window is the last hour and a day becomes sealable an hour after it ends, after its last minutes have passed the engine's lookahead and become final. A minute the server still refuses as "already sealed" can't be retried and its credit is lost; it is counted (`crediting.sealed_rejected`) and shown in status as an error, never dropped silently. It points to a bug.

**Evidence days.** A day is sealed once it has ended plus one hour and its minutes are in (above), in order, at most 7 per run, starting at the device's registration day (and no earlier than the 90-day retention). Days with no events are skipped: a gap, which the server allows. A sealed day is stored as `pending` before upload, so retries send the same signed values. The chain advances only when the server answers 200/201. A 409 (conflict or chain mismatch) or 400 marks the day `conflict`/`rejected` and stops the chain; status shows it. The agent never re-seals a refused day or starts a second chain for the same device. A local failure while sealing (the database, the active-time source, encoding) is not a server problem: it is shown as `evidence.local_error` and retried on the next run, without backing off minute uploads. Each sealed day keeps its seal time (`evidence_days.created_at`), so a bundle can show how late a day was sealed.

**Backoff and pausing** follow the heartbeat uploader: exponential backoff with jitter (5 s to 10 min, a longer `Retry-After` wins, capped at 30 min), and on 401 a pause until the API key changes, with one retry every 30 minutes. A 403, 404 or 409 without a JSON error body (a school filter or proxy) is retried, never read as "revoked", "unknown" or "conflict". Only server answers drive backoff.

**Performance.** One run at a time. The window's events are cached in memory. The first run after a start fills the cache with a time-indexed read of the window; later runs read only rows added since, by id, so late events are still seen. On the dev VPS (`BenchmarkRunOnce`, two weeks of history plus a busy two-hour session) a warm run takes about 3 ms and a cold run about 17 ms, mostly statement preparation, against a 50 ms budget. Walking ids from zero instead took 42 ms on that database and grows with it.

**Merge dependency.** The signals module adds `ai_events.prompt_hash` (local migration 0003). When it is merged, the AI-event reads (`qAIRange`, `qAIAfter`, the scan in `readEvents`) must select it too, or the engine's repeated-prompt cut never fires on the device and evidence leaves the field out. `TestAIEventPromptHashRoundTrip` fails until they do.

## Consequences

- The evidence format is pinned by tests in `pkg/evidence`, and agent and server share one implementation.
- A step 5 bundle must contain the same rows (by the stored high-water ids) and the stored active-time records (`ReplayDay`), including file paths and working directories. Redacting them would change the hash; if redaction is wanted, it needs a new encoding version.
- A conflict needs a person to look at it. Logging in with a new API key starts a new device and chain.
- If the agent's clock is far ahead of the server's, rewrites of open minutes may be refused as final; they are counted, not retried.
