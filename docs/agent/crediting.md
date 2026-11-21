# Agent crediting and evidence

How the agent runs the struggle engine and keeps the evidence chain (ADR 0010, ADR 0013, ADR 0017). The rules themselves are in `docs/STRUGGLE-RULES.md`.

| Package | Job |
|---|---|
| `pkg/evidence` | Canonical encoding of logged rows (`Encode`, `ContentHash`, `Counts`), chain hash, signing |
| `internal/agent/devicekey` | The device's ed25519 key file and its registration with the server |
| `internal/agent/serverapi` | HTTP client for `/api/v1/devices`, `/minutes`, `/evidence-days` |
| `internal/agent/crediting` | The per-minute pipeline: engine input, local minutes and their keys, uploads, sealing blocks, replays |
| `internal/agent/localdb` | The event log (`seq` on every raw event table and `repo_map`, migration 0007), snapshots, `credited_minutes`, `final_spans`, `evidence_days` |

## Files and tables

| What | Where | Bound |
|---|---|---|
| Device key | `<data dir>/device.key`, PKCS#8 PEM, 0600 | one file; retired keys are kept as `device.key.retired-<unix>[-n]`, never overwritten |
| Event log | `heartbeats` (payload as received, deduplicated), `ai_events`, `activity_samples`, `git_events`, `active_time`, `repo_map`; every row has a unique `seq` from `kv` `log.seq` | 90 days and 3 000 000 rows per table, but only rows sealed by the device's chain (`kv` `log.sealed`) are deleted; with the table full of unsealed rows, new activity samples are dropped and counted (`log.dropped_samples`), other rows are always kept |
| Credited minutes | `credited_minutes` (minute, project, label, credit, cuts, flags, rules version, key `computed_at`/`snapshot`, `open`/`final`, acknowledged, what the server holds) | 90 days |
| Final minutes | `final_spans` (merged ranges, also covering final minutes with no rows) | 90 days |
| Evidence days | `evidence_days` (signed values, event counts, sequence range `seq_from`..`seq_to`, state; days sealed before migration 0007 are `legacy`) | one row per day, kept |
| State | `kv` keys `device.*`, `crediting.*`, `evidence.*`, `log.*` | small |

## The event log

Every row the engine can read, and every repo mapping row, takes the next sequence number inside its inserting transaction. SQLite serialises writers, so the committed rows are exactly `1..H` (less what retention deleted). An ignored duplicate (a resent heartbeat, a commit seen again, a retried OTLP export, an unchanged repo mapping) takes no number. Logged rows are never updated. A reader opens one read transaction and reads `H` first (`localdb.DB.View`); a write that hasn't committed by then is entirely outside it.

## Each minute

1. Log the active repos (a `repo_map` row when a path is first seen or its project changes).
2. Open one snapshot of the log: read `H`, the deletion generation, the events of the last 120 minutes into the in-memory window (only rows logged since the last run, by `seq`, plus older rows the window's end has just reached; refilled when the window jumps or the generation moved) and the repo mapping at `H`. Count the rows logged since the last run whose minute is already final ("late events").
3. Run `struggle.Compute` once for the window `[T−60 min, T)`, `T` being the whole minute now is in, with 60 minutes of history, `DataEnd = T`, the mapping at `H` and optional Claude Code active time (flags only). The run's **key** is `(T, H)`.
4. Store the result (`localdb.SaveRun`). A final minute is never touched. For an open minute, if the new rows (plus a removal for each project the server may hold that dropped out) equal the stored rows, the stored rows and their older key are kept, so it isn't sent again; otherwise all of the minute's rows are replaced under the new key. Minutes before the end of the last sealed day (`crediting.sealed_through`) are never stored.
5. If the agent has **settled**, every open minute `m` with `T ≥ m + 29 min` becomes final in the same write (and the minutes of the window up to there, with or without rows). After a start, a wake (a gap since the last run longer than two intervals plus a minute, by the wall clock or the monotonic clock) nothing becomes final until the agent has been up `Settle` (15 minutes) and the startup spool drain has finished. This is about fairness only: late honest events still count. Whatever the clock does later, a final minute is never computed again.
6. If signed in: register the device if needed, upload every row the server hasn't answered for, with its key (batches of whole minutes, at most 1000 rows and 20 batches a run), except open minutes the server would freeze before the agent settles (held). Then seal and upload ended days.

`crediting.ReplayMinute(m, key)` runs the engine at a minute's key over the rows with `seq ≤ H` and compares on label, project, credit and cuts (flags depend on the time zone and active time). `crediting.ReplayBlock` re-reads a sealed block's rows for re-hashing.

Runs never overlap, are bounded to 2 minutes, and run a few seconds past each minute. The first run waits (at most 2 minutes) for the daemon's startup spool drain. Old rows are pruned every 6 hours. On the dev VPS (`BenchmarkRunOnce`: two weeks of history plus a busy two-hour session, under load) a warm run takes about 10 ms and a cold one about 20 ms.

**Clock behind a sealed day.** If the system clock is before the end of the last day the evidence chain sealed or skipped (it was set back), a run does nothing: no computing, uploading or sealing, since every minute it computed would fall in a day the server already refuses. Status shows "clock: the system clock is behind the last sealed day ..." and one log line says so; when the clock passes that day, crediting resumes by itself (one more log line).

## Uploads

All requests use `Authorization: Bearer <api key>` and the agent's User-Agent, through the daemon's shared HTTP client. Before a batch is sent its rows are marked as possibly held by the server, so if the answer is lost, a later result without a project still sends that project's removal.

| Answer | Action |
|---|---|
| 200/201, item accepted | the row is acknowledged (the server holds it, key included); an accepted removal is deleted locally too |
| item refused as final, with `stored` rows | the minute takes exactly the server's rows (values and keys), final and acknowledged; rows without a key are counted (`crediting.unkeyed`) |
| any other per-item rejection | counted in `crediting.rejected`; an open row is not retried while open (it is sent again once final); a final row is sent again on the next run until the server takes it or answers with its rows |
| per-item rejection "already sealed" | counted, also in `crediting.sealed_rejected`, shown in status as an error (lost credit; should never happen), not retried |
| 413, or 400 "minutes per request", on a minutes batch | split the batch (never inside a minute); a single refused minute is counted as rejected |
| any other 400/422 with a JSON error on a minutes batch (e.g. unknown `rules_version`) | minutes kept and put on hold for 30 min; status shows "on hold" with the server's reason |
| 400 or 409 on an evidence day | day marked `rejected`/`conflict`; the chain stops; status shows it |
| 401 | pause until the API key changes; one retry every 30 min |
| 403 with a JSON error | device revoked: no more uploads until the API key changes, then a new key and device. If the current API key never worked with the device (revoked, then `logbook login`), a new key and device at once |
| 404 with a JSON error | device unknown: forget the id, back off, register the same key again |
| 409 "public key is registered to another account" on registration | retire the key, make a new one, register |
| any other 409 with a JSON error on registration (device limit) | keep the key, show the message as the pause reason, back off |
| anything else (network, 429, 5xx, 3xx, proxy pages, a lost answer) | back off 5 s → 10 min with jitter; `Retry-After` wins (≤ 30 min); the rows are sent again with the same key, which the server takes as a repeat |

A local failure while sealing a day (database, signing) is shown as "sealing failed" and retried next run; it does not back off uploads.

## Evidence days

The record for UTC day `D` seals every logged row with `seq` in `(H_prev, H_D]`: `H_prev` is where the device's previous block ended (a device's first block, or the first after a legacy day, starts at the oldest retained row), and `H_D` is the log's high-water mark read in the snapshot that seals. A block holds events of any time: a late event for an earlier day goes into whichever block is open when it arrives, so every row is sealed exactly once. A range with no rows is skipped (a gap). The rows are read and hashed in chunks of sequence numbers (`evidence.Hasher`, at most 25 chunks of 20,000 a run), so a large block, such as a device's first, is hashed over several runs without holding it in memory; the progress is kept in `kv` (`evidence.progress`) for that device and previous block. The hash is chained to the last accepted day (`prev_chain_hash`, `""` for the first), signed with the device key and stored as `pending` with `seq_from` and `seq_to`; it is uploaded until the server accepts it, and only then does the chain move on. Sealing moves `log.sealed`, which lets retention delete those rows later. A new device resets it to 0.

A day is sealed once it has ended plus one hour by the earlier of the system clock and the server's, the two agree to within 5 minutes, every stored minute up to its end is final and acknowledged (so every key of its minutes is at or below `H_D`), and this run's minute uploads went through. The server's clock is the `server_time` in the body of a minutes response in the same run (an empty batch asks for it when the run sent none); the `Date` header is never used. If `server_time` is missing, malformed or disagrees, no day is sealed or skipped and `crediting.sealed_through` doesn't move; status says why, minutes are still computed and uploaded, and sealing catches up in order once the clocks agree. Catch-up after downtime seals up to 7 days per run, in order.

## Status

`logbook status`, `GET /local/v1/status` (`crediting` object) and the status page show: device id and registration time, revocation, when minutes were last computed and uploaded, minutes waiting and rejected, pause reason and last error, a clock problem (behind the last sealed day, more than 5 minutes from the server's clock, or no usable server time; the last two stop sealing), whether minutes are settling after a start or wake (`settling`), how many events were logged after their minute was final (`late_ignored`, `late_latest`), minutes adopted from the server without a key (`unkeyed`), activity samples dropped because the log was full of unsealed rows (`dropped_samples`), and the evidence chain's last day, chain hash and any stop reason. They never show the API key or the device's private key.

## What leaves the device

Only labelled minutes (minute, project, label, credited seconds, cut and flag codes, rules version, and the computation key: a window end and a sequence number) and per-day signed hashes with row counts. Raw events, prompts (or their hashes), working directories, window titles and file paths stay local unless the user submits a bundle (step 5).
