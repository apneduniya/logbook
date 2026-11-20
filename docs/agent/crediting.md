# Agent crediting and evidence

How the agent runs the struggle engine and keeps the evidence chain (ADR 0010, ADR 0013). The rules themselves are in `docs/STRUGGLE-RULES.md`.

| Package | Job |
|---|---|
| `pkg/evidence` | Canonical day encoding (`Encode`, `ContentHash`, `Counts`), chain hash, signing |
| `internal/agent/devicekey` | The device's ed25519 key file and its registration with the server |
| `internal/agent/serverapi` | HTTP client for `/api/v1/devices`, `/minutes`, `/evidence-days` |
| `internal/agent/crediting` | The per-minute pipeline: engine input, local minutes, uploads, sealing days |
| `internal/agent/localdb` | Tables `heartbeats`, `credited_minutes`, `evidence_days` (migrations 0002, 0004), `repo_map` (0005), `credit_cutoffs` and the `arrived_at`/`cutoff` columns (0006) |

## Files and tables

| What | Where | Bound |
|---|---|---|
| Device key | `<data dir>/device.key`, PKCS#8 PEM, 0600 | one file; retired keys are kept as `device.key.retired-<unix>[-n]`, never overwritten |
| Full heartbeats | `heartbeats` (payload as received, deduplicated) | 90 days, 3 000 000 rows |
| Credited minutes | `credited_minutes` (minute, project, label, credit, cuts, flags, rules version, upload state, arrival cutoff) | 90 days |
| Final minutes' cutoffs | `credit_cutoffs` (ranges of final minutes whose cutoff isn't minute + 29 min; 0 = never computed) and `kv` `crediting.final_through` | 90 days |
| Evidence days | `evidence_days` (signed values, event counts, high-water ids, the active-time records hashed, the day's cutoffs, state) | one row per day, kept; active-time records and cutoffs cleared after 90 days |
| Repo mapping journal | `repo_map` (since, repository path, project), append-only | a row is pruned once a newer row for its path is 90 days old |
| State | `kv` keys `device.*`, `crediting.*`, `evidence.*` | small |

## Each minute

1. Read new local events since the last run (by row id) into an in-memory window of the last 120 minutes, plus older rows whose time the window's end has just reached. If the window moves backward or jumps past its previous end (the clock was changed), or rows were pruned or trimmed (the deletion generation `events.generation` changed), it is read again from scratch, so it always holds exactly what a full read would.
2. Record the active repos in the repo mapping journal (`repo_map`), then run `struggle.Compute` for `[now−60 min, now)` over the events that arrived by the run's **cutoff** (the window's end, a whole minute; ADR 0016), with optional Claude Code active time and, for each minute, the repo mapping recorded before that minute became final (`ReposAt`; one engine call over the whole window per group of minutes sharing a mapping, with `DataEnd` at the window's end, keeping that group's minutes).
3. Store the result, each row with the run's cutoff: minutes that aren't final are rewritten when they change; final minutes are frozen whole. Minutes before the end of the last sealed day (`crediting.sealed_through`) are never stored.
4. Make minutes final (**settled finality**, ADR 0016). Normally the run at minute + 29 minutes does it: the watermark `crediting.final_through` moves past the minute. After a start, or a gap in runs (a sleep), nothing becomes final until the agent has been up `Settle` (15 minutes) and the startup spool drain has finished; minutes are still computed for the status page. A final minute keeps the cutoff it was computed with; cutoffs other than minute + 29 minutes are recorded in `credit_cutoffs`. A minute the server already froze stays as the server has it. Events that arrive after their minute is final are kept but don't change it; status counts them ("late events ignored").
5. If signed in: register the device if needed, upload minutes the server doesn't have (batches of 1000, at most 20 a run), except minutes held while settling (the server would freeze them before they are final here), then seal and upload ended days whose minutes are all final and answered for.

`crediting.ReplayMinutes` and `ReplayDayCredit` recompute final minutes from the raw events, the repo journal and the stored cutoffs exactly as the agent made them final.

Runs never overlap, are bounded to 2 minutes, and run a few seconds past each minute. The first run waits (at most 2 minutes) for the daemon's startup spool drain. Old heartbeats and minutes are pruned every 6 hours.

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

`logbook status`, `GET /local/v1/status` (`crediting` object) and the status page show: device id and registration time, revocation, when minutes were last computed and uploaded, minutes waiting and rejected, pause reason and last error, a clock problem (behind the last sealed day, more than 5 minutes from the server's clock, or no usable server time; the last two stop sealing), whether minutes are settling after a start (`settling`), how many events arrived after their minute was final (`late_ignored`, `late_latest`; the status page and JSON), and the evidence chain's last day, chain hash and any stop reason. They never show the API key or the device's private key.

## What leaves the device

Only labelled minutes (minute, project, label, credited seconds, cut and flag codes, rules version) and per-day signed hashes with event counts. Raw events, prompts (or their hashes), working directories, window titles and file paths stay local unless the user submits a bundle (step 5).
