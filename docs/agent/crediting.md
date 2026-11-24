# Agent crediting and evidence

How the agent runs the struggle engine and keeps the evidence chain (ADR 0010, ADR 0013, ADR 0017, ADR 0019). The rules themselves are in `docs/STRUGGLE-RULES.md`.

| Package | Job |
|---|---|
| `pkg/evidence` | Canonical line of a logged row, salted minute buckets under a Merkle root (`Hasher`, `HashBlock`, `Bucket.Check`), proofs and completeness (`BuildProof`, `VerifyProof`, `PlanProof`), chain hash, signing |
| `pkg/replay` | Engine input for a computation key from logged rows; what replays compare. Shared by live runs, `ReplayMinute` and the server's re-check |
| `internal/agent/devicekey` | The device's ed25519 key file and its registration with the server |
| `internal/agent/serverapi` | HTTP client for `/api/v1/devices`, `/minutes`, `/evidence-days` |
| `internal/agent/crediting` | The per-minute pipeline: engine input, local minutes and their keys, uploads, sealing blocks, replays, proofs for submissions |
| `internal/agent/localdb` | The event log (`seq` on every raw event table and `repo_map`, migration 0007), snapshots, `credited_minutes`, `final_spans`, `evidence_days` and the sealing state (migration 0008) |

## Files and tables

| What | Where | Bound |
|---|---|---|
| Device key | `<data dir>/device.key`, PKCS#8 PEM, 0600 | one file; retired keys are kept as `device.key.retired-<unix>[-n]`, never overwritten |
| Event log | `heartbeats` (payload as received, deduplicated), `ai_events`, `activity_samples`, `git_events`, `active_time`, `repo_map`; every row has a unique `seq` from `kv` `log.seq` | 90 days and 3 000 000 rows per table, but only rows sealed by the device's chain (`kv` `log.sealed`) are deleted; with the table full of unsealed rows, new activity samples are dropped and counted (`log.dropped_samples`), other rows are always kept |
| Credited minutes | `credited_minutes` (minute, project, label, credit, cuts, flags, rules version, key `computed_at`/`snapshot`, `open`/`final`, acknowledged, what the server holds) | 90 days |
| Final minutes | `final_spans` (merged ranges, also covering final minutes with no rows) | 90 days |
| Evidence days | `evidence_days` (signed values, event counts, sequence range `seq_from`..`seq_to`, bucket count and minute span, state; days sealed before migration 0007 are `legacy`, days sealed before migration 0008 have no bucket list) | one row per day, kept |
| Bucket lists | `evidence_buckets` (per sealed block: each leaf's index, key, salt and bucket hash) | about 100 KB for a busy day, kept as long as the chain |
| Sealing state | `evidence_sealing` (the block being sealed: day, frozen `seq_to`, hashing cursor and hasher state), `evidence_baselines` (where a device that took over starts its chain) | one row each |
| State | `kv` keys `device.*`, `crediting.*`, `evidence.*`, `log.*` | small |

## The event log

Every row the engine can read, and every repo mapping row, takes the next sequence number inside its inserting transaction. SQLite serialises writers, so the committed rows are exactly `1..H` (less what retention deleted). An ignored duplicate (a resent heartbeat, a commit seen again, a retried OTLP export, an unchanged repo mapping) takes no number. Logged rows are never updated. A reader opens one read transaction and reads `H` first (`localdb.DB.View`); a write that hasn't committed by then is entirely outside it.

## Each minute

1. Log the active repos (a `repo_map` row when a path is first seen or its project changes).
2. Open one snapshot of the log: read `H`, the deletion generation, the events of the last 120 minutes into the in-memory window (only rows logged since the last run, by `seq`, plus older rows the window's end has just reached; refilled when the window jumps or the generation moved) and the repo mapping at `H`. Count the rows logged since the last run whose minute is already final ("late events").
3. Run `struggle.Compute` once for the window `[T−60 min, T)`, `T` being the whole minute now is in, with 60 minutes of history, `DataEnd = T`, the mapping at `H` and optional Claude Code active time (flags only). The run's **key** is `(T, H)`. The input is built by `pkg/replay` (`InputWithRepos`), the same code every replay of the minute runs; only the active time is added on top.
4. Store the result (`localdb.SaveRun`). A final minute is never touched. For an open minute, if the new rows (plus a removal for each project the server may hold that dropped out) equal the stored rows, the stored rows and their older key are kept, so it isn't sent again; otherwise all of the minute's rows are replaced under the new key. Minutes before the end of the last sealed day (`crediting.sealed_through`) are never stored.
5. If the agent has **settled**, every open minute `m` with `T − ahead ≥ m + 29 min` becomes final in the same write (and the minutes of the window up to there, with or without rows). After a start, a wake (a gap since the last run longer than two intervals plus a minute, by the wall clock or the monotonic clock) nothing becomes final until the agent has been up `Settle` (15 minutes) and the startup spool drain has finished; this is about fairness only: late honest events still count. Nor does anything become final until a minutes response since then has carried the server's `server_time` (an empty batch asks for it): `ahead` is how far the system clock was ahead of it (0 when behind), so a clock running ahead never makes minutes final that haven't happened yet. Whatever the clock does later, a final minute is never computed again.
6. If signed in: register the device if needed, upload every row the server hasn't answered for, with its key (batches of whole minutes, at most 1000 rows and 20 batches a run), except open minutes the server would freeze before the agent settles (held). Then seal and upload ended days.

When a new device replaces the previous one (a new id or key), the minutes before it registered belong to the previous device (`localdb.RetireMinutesBefore`): they become final, rows that device holds exactly as stored stay, and every other row is dropped. None is ever sent again. Its evidence chain starts at the log's high-water mark then (`localdb.StartChain`, one transaction): that is its first block's `seq_from`, a repo snapshot (below) becomes its first rows, and a block the previous device was still hashing is dropped.

`crediting.ReplayMinute(m, key)` runs the engine at a minute's key over the rows with `seq ≤ H`, with the input built by `pkg/replay`, and compares on label, project, credit and cuts (`replay.Same`; flags depend on the time zone and active time). `crediting.ReplayBlock` re-reads a sealed block's rows and re-hashes them with the stored bucket salts: they must give the stored bucket list and its root, the signed content hash (`ErrBlockMismatch` otherwise; `ErrNotReproducible` when rows were pruned since, or the block was sealed before the agent kept bucket lists).

Runs never overlap, are bounded to 2 minutes, and run a few seconds past each minute. The first run waits (at most 2 minutes) for the daemon's startup spool drain. Old rows are pruned every 6 hours. On the dev VPS (`BenchmarkRunOnce`: two weeks of history plus a busy two-hour session, under load) a warm run takes about 10 ms and a cold one about 20 ms.

**Clock behind a sealed day.** If the system clock is before the end of the last day the evidence chain sealed, reserved for sealing or skipped (it was set back), a run does nothing: no computing, uploading or sealing, since every minute it computed would fall in a day the server already refuses. Status shows "clock: the system clock is behind the last sealed day ..." and one log line says so; when the clock passes that day, crediting resumes by itself (one more log line).

## Uploads

All requests use `Authorization: Bearer <api key>` and the agent's User-Agent, through the daemon's shared HTTP client. Before a batch is sent its rows are marked as possibly held by the server, so if the answer is lost, a later result without a project still sends that project's removal.

| Answer | Action |
|---|---|
| 200/201, item accepted | the row is acknowledged (the server holds it, key included); an accepted removal is deleted locally too |
| item refused as final, with `stored` rows | the minute takes exactly the server's rows (values and keys), final and acknowledged; rows without a key are counted (`crediting.unkeyed`) |
| any other per-item rejection of an open row | not retried while open; sent once more when final |
| any other per-item rejection of a final row | refused for good: the row is dropped (the minute stays final) and counted in `crediting.rejected`; never retried |
| per-item rejection "already sealed" | the row is dropped, its minute made final, and counted, also in `crediting.sealed_rejected`; shown in status as an error (lost credit; should never happen) |
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

The record for UTC day `D` seals every logged row with `seq` in `(seq_from, seq_to]`. `seq_from` is where the device's previous block ended (0 for a device's first block on a first install, or after a legacy day; for a device that took over, the high-water mark when it did), and `seq_to` is the log's high-water mark when `D`'s bound was frozen. A block holds events of any time: a late event for an earlier day goes into whichever block is open when it arrives, so every row is sealed exactly once. The rows are grouped into salted buckets, a repo bucket and one per minute, under a Merkle root (ADR 0019), and the record carries `seq_from`, `seq_to`, the bucket count and the minute span.

Sealing a day takes three steps (ADR 0019 point 4):

1. **Reserve** (`localdb.ReserveSeal`, one transaction, once the conditions below hold): read the high-water mark and store it as the block's `seq_to`; log every path's latest mapping again as new `repo_map` rows (a repo snapshot, even where nothing changed), which take the next numbers and so open the next range; and mark the day sealed (`crediting.sealed_through`, and `evidence.sealing` for status). From then on no run computes or stores a minute of `D` or earlier, across restarts and whatever the clock does, so every minute of the day has a key at or below `seq_to`. A range with no rows but repo rows (such as the last snapshot) is not reserved: the day is a gap, and the range goes on into the next block.
2. **Hash** (`crediting.hashBlock`): the repo bucket first, then the minute buckets an hour at a time, in `(bucket key, seq)` order, reading only rows with `seq_from < seq ≤ seq_to`, so a row logged meanwhile goes into the next block and the cursor can't miss one. A run hashes at most 200 000 rows (`Options.SealRows`); after each run the buckets it closed (key, salt, bucket hash), the cursor and the hasher's state (the Merkle frontier and the open bucket's running hash) are saved in one transaction, so a large first block (up to 90 days) takes several runs and a restart resumes where it stopped. Salts are fresh random 32-byte values.
3. **Complete** (`localdb.CompleteSeal`, one transaction): check the bucket list against the root, sign the record (chained to the last accepted day: `prev_chain_hash`, `""` for the first), and store it as `pending` with its bucket list. Only now does `log.sealed` move to `seq_to`, so retention never deletes rows of a range before its record and bucket list are stored. A new device resets it to 0.

A pending day is uploaded with the fields it was signed with until the server accepts it, and only then does the chain move on. Local migration 0008 drops a day sealed before it that the server hadn't accepted (pending, rejected or in conflict: it can't be sent without a bucket list, and the new server refuses the old record) so it is sealed again, and clears the refusal shown in status.

A day is reserved once it has ended plus one hour by the earlier of the system clock and the server's, the two agree to within 5 minutes, every stored minute up to its end is final and acknowledged (so every key of its minutes is at or below `seq_to`), and this run's minute uploads went through. A block already reserved is hashed and sent whatever the clocks say. The server's clock is the `server_time` in the body of a minutes response in the same run (an empty batch asks for it when the run sent none); the `Date` header is never used. If `server_time` is missing, malformed or disagrees, no day is sealed or skipped and `crediting.sealed_through` doesn't move; status says why, minutes are still computed and uploaded, and sealing catches up in order once the clocks agree. Catch-up after downtime seals up to 7 days per run, in order.

## Proofs

`crediting.BuildProofs(keys)` builds what `logbook submit` uploads for the minutes computed at the given keys (ADR 0020), from the log and the stored bucket lists in one snapshot. For each key it takes the window its run read, `[T − 120 min, T)`. Each accepted block whose signed minute span overlaps a window and whose `seq_from` is below the key's `H` opens every minute bucket in it, with the leaves just before and after as edges (completeness, ADR 0019 point 6); the block holding `H` also opens its repo bucket. Hidden buckets are proven from the bucket list alone, so pruning their rows doesn't matter. A key is **unbacked** (its minutes go without evidence) when no accepted block holds its `H`, when its window read rows outside the device's provable blocks (logged before its chain started, or in a day sealed before bucket lists were kept), or when a bucket it needs opened has lost rows to pruning.

## Status

`logbook status`, `GET /local/v1/status` (`crediting` object) and the status page show: device id and registration time, revocation, when minutes were last computed and uploaded, minutes waiting and rejected, pause reason and last error, a clock problem (behind the last sealed day, more than 5 minutes from the server's clock, or no usable server time; the last two stop sealing), whether minutes are settling after a start or wake, or waiting for the server's time (`settling`), how many events were logged after their minute was final (`late_ignored`, `late_latest`), minutes adopted from the server without a key (`unkeyed`), activity samples dropped because the log was full of unsealed rows (`dropped_samples`), and the evidence chain's last day, chain hash, any stop reason and the day being sealed, if any (`evidence.sealing`: closed to new minutes, its block still being hashed). They never show the API key or the device's private key.

## What leaves the device

Only labelled minutes (minute, project, label, credited seconds, cut and flag codes, rules version, and the computation key: a window end and a sequence number) and per-day signed hashes with row counts. Raw events, prompts (or their hashes), working directories, window titles and file paths stay local unless the user submits a bundle (step 5).
