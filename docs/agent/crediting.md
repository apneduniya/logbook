# Agent crediting and evidence

How the agent runs the struggle engine and keeps the evidence chain (ADR 0010, ADR 0013). The rules themselves are in `docs/STRUGGLE-RULES.md`.

| Package | Job |
|---|---|
| `pkg/evidence` | Canonical day encoding (`Encode`, `ContentHash`, `Counts`), chain hash, signing |
| `internal/agent/devicekey` | The device's ed25519 key file and its registration with the server |
| `internal/agent/serverapi` | HTTP client for `/api/v1/devices`, `/minutes`, `/evidence-days` |
| `internal/agent/crediting` | The per-minute pipeline: engine input, local minutes, uploads, sealing days |
| `internal/agent/localdb` | Tables `heartbeats`, `credited_minutes`, `evidence_days` (migrations 0002, 0004), `repo_map` (0005) |

## Files and tables

| What | Where | Bound |
|---|---|---|
| Device key | `<data dir>/device.key`, PKCS#8 PEM, 0600 | one file; retired keys are kept as `device.key.retired-<unix>[-n]`, never overwritten |
| Full heartbeats | `heartbeats` (payload as received, deduplicated) | 90 days, 3 000 000 rows |
| Credited minutes | `credited_minutes` (minute, project, label, credit, cuts, flags, rules version, upload state) | 90 days |
| Evidence days | `evidence_days` (signed values, event counts, high-water ids, the active-time records hashed, state) | one row per day, kept; active-time records cleared after 90 days |
| Repo mapping journal | `repo_map` (since, repository path, project), append-only | a row is pruned once a newer row for its path is 90 days old |
| State | `kv` keys `device.*`, `crediting.*`, `evidence.*` | small |

## Each minute

1. Read new local events since the last run (by row id) into an in-memory window of the last 90 minutes.
2. Record the active repos in the repo mapping journal (`repo_map`), then run `struggle.Compute` for `[now−60 min, now)`, with optional Claude Code active time and, for each minute, the repo mapping recorded before that minute became final (`ReposAt`; one engine call over the whole window per group of minutes sharing a mapping, with `DataEnd` at the window's end, keeping that group's minutes).
3. Store the result: open minutes (< 30 min old) are rewritten when they change; a final minute is frozen whole and only filled in if it has no rows at all. Minutes before the end of the last sealed day (`crediting.sealed_through`) are never stored.
4. If signed in: register the device if needed, upload minutes the server doesn't have (batches of 1000, at most 20 a run), then seal and upload ended days whose minutes the server has all answered for.

Runs never overlap, are bounded to 2 minutes, and run a few seconds past each minute. Old heartbeats and minutes are pruned every 6 hours.

**Clock behind a sealed day.** If the system clock is before the end of the last day the evidence chain sealed or skipped (it was set back), a run does nothing: no computing, uploading or sealing, since every minute it computed would fall in a day the server already refuses. Status shows "clock: the system clock is behind the last sealed day ..." and one log line says so; when the clock passes that day, crediting resumes by itself (one more log line).

## Uploads

All requests use `Authorization: Bearer <api key>` and the agent's User-Agent, through the daemon's shared HTTP client.

| Answer | Action |
|---|---|
| 200/201 | done |
| per-minute rejection | logged, counted in `crediting.rejected`, never retried unless the minute changes |
| per-minute rejection "already sealed" | as above, and also counted in `crediting.sealed_rejected` and shown in status as an error (lost credit; should never happen) |
| 413, or 400 "minutes per request", on a minutes batch | split the batch (never inside a minute); a single refused minute is counted as rejected |
| any other 400/422 with a JSON error on a minutes batch (e.g. unknown `rules_version`) | minutes kept and put on hold for 30 min; status shows "on hold" with the server's reason |
| 400 or 409 on an evidence day | day marked `rejected`/`conflict`; the chain stops; status shows it |
| 401 | pause until the API key changes; one retry every 30 min |
| 403 with a JSON error | device revoked: no more uploads until the API key changes, then a new key and device. If the current API key never worked with the device (revoked, then `logbook login`), a new key and device at once |
| 404 with a JSON error | device unknown: forget the id, back off, register the same key again |
| 409 "public key is registered to another account" on registration | retire the key, make a new one, register |
| any other 409 with a JSON error on registration (device limit) | keep the key, show the message as the pause reason, back off |
| anything else (network, 429, 5xx, 3xx, proxy pages) | back off 5 s → 10 min with jitter; `Retry-After` wins (≤ 30 min) |

A local failure while sealing a day (database, active-time source) is shown as "sealing failed" and retried next run; it does not back off uploads.

## Evidence days

A UTC day is sealed once it has ended plus one hour by the earlier of the system clock and the server's, and the server has answered for every stored minute up to its end. The server's clock is the `server_time` in the body of a minutes response in the same run (an empty batch asks for it when the run sent none); the `Date` header is never used, since a proxy may strip or rewrite it. If `server_time` is missing or malformed, or differs from the system clock by more than 5 minutes, no day is sealed or skipped as empty and `crediting.sealed_through` doesn't move; status says why, minutes are still computed and uploaded, and sealing catches up in order once the clocks agree. sealing closes the day to minutes on the server. No new day is sealed on a run whose minute uploads are on hold or failed. The agent records the highest row id of each event table and keeps the day's active-time records as hashed (they can't be pinned by ids), hashes the day's events up to those ids plus those records (`evidence.ContentHash`), chains it to the last accepted day (`prev_chain_hash`, `""` for the first), signs it with the device key and stores it as `pending`. It is uploaded until the server accepts it; only then does the chain move on. Days without events are skipped. Catch-up after downtime seals up to 7 days per run, in order. `crediting.ReplayDay` rebuilds a sealed day's events from the same marks and records, so late events never change its hash (ADR 0013).

## Status

`logbook status`, `GET /local/v1/status` (`crediting` object) and the status page show: device id and registration time, revocation, when minutes were last computed and uploaded, minutes waiting and rejected, pause reason and last error, a clock problem (behind the last sealed day, more than 5 minutes from the server's clock, or no usable server time; the last two stop sealing), and the evidence chain's last day, chain hash and any stop reason. They never show the API key or the device's private key.

## What leaves the device

Only labelled minutes (minute, project, label, credited seconds, cut and flag codes, rules version) and per-day signed hashes with event counts. Raw events, prompts (or their hashes), working directories, window titles and file paths stay local unless the user submits a bundle (step 5).
