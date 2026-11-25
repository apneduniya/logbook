# Submissions and review API

How a user submits a project's hours, how the server re-checks them, and what reviewers read (ADR 0019, ADR 0020). Packages: `internal/server/submissions` (upload, storage, the re-check and retention jobs, serving the packet, decisions, each program's inbox), `pkg/submission` (the wire types below, the verifier, the cap, the packet builder and the offline bundle check, shared with the agent and `logbook verify`), `pkg/replay`, `pkg/evidence`.

## Conventions

As in [devices.md](devices.md): API-key auth for the agent, session auth (plus `X-CSRF-Token` on writes) for the browser, errors as `{"error": "message"}`. Times are RFC 3339 UTC; days are `YYYY-MM-DD` UTC.

- A submission `{id}` that isn't a UUID, doesn't exist, or isn't the caller's is `404 {"error": "submission not found"}`.
- **Rate limits** per user: `POST /api/v1/submissions` and `POST /api/v1/submissions/{id}/parts` share a burst of 10, refilled at 10 per hour. `GET /api/v1/me/submissions/plan`: a burst of 60, refilled at 1 per second. `POST /api/v1/review/{token}/decision`: a burst of 30, refilled at 30 per minute. `GET /api/v1/review/{token}`: per IP, a burst of 120, refilled at 120 per minute. `GET /api/v1/submissions*`: per token, a burst of 600, refilled at 600 per minute (ADR 0021). Over the limit: `429` with `Retry-After`.
- **CORS:** the agent's routes (`/api/v1/me/submissions/plan`, `POST /api/v1/submissions*`) and the program's client-credentials routes (`GET /api/v1/submissions*`) answer no CORS; the review link, decisions and the user's and members' lists keep the web origin's CORS.
- Every answer that can carry a review link (`/api/v1/me/submissions*`, the program lists, `POST /api/v1/submissions`) is `Cache-Control: no-store`. No log line records a review token: every request under `/api/v1/review/` (matched on the cleaned path, in any case) is logged as `/api/v1/review/{token}/…`, whether a route answered it or not (a CORS preflight, a `404`, a `405`, a redirect), and so is the path of a `5xx`'s error line (`httpapi.LoggedPath`).

## Status values

| `status` | Meaning |
|---|---|
| `verifying` | Uploaded; the re-check job hasn't finished |
| `ready` | Re-checked; the packet is built. Counts can include `mismatch` and `unbacked` minutes |
| `failed` | The bundle was invalid (a proof didn't match, a block is missing). `error` says why. Nothing is shown to reviewers |
| `withdrawn` | The submitter withdrew it before any decision |

Decision values: `approved`, `adjusted`, `rejected`, or `null` when none.

---

## `GET /api/v1/me/submissions/plan`

Auth: API key. What the agent needs to build a bundle.

Query: `project` (required), `from`, `to` (days, inclusive; at most 92 days).

`200`:
```json
{
  "project": "terra-bot",
  "from": "2026-11-01", "to": "2026-11-20",
  "devices": [
    {"id": "<uuid>", "name": "laptop", "sealed_through": "2026-11-24", "this_device": true, "revoked": false}
  ],
  "minutes": [
    {"device_id": "<uuid>", "minute": "2026-11-03T14:21:00Z", "project": "terra-bot",
     "label": "you", "credited": 60, "cuts": [], "flags": [], "rules_version": "v1",
     "computed_at": "2026-11-03T14:30:00Z", "snapshot": 48213}
  ]
}
```
- `submission.Plan` in Go. `minutes` are the rows the server holds for this project and range, every device and every label (cut and idle rows too), sorted by (device, minute). Rows stored before ADR 0017 have no key: `computed_at` and `snapshot` are `null`, and they can't be backed.
- `devices` are the devices with a row in the range, by id; `sealed_through` is the latest day the device has an evidence record for (any record), or `null` for a device that has sealed no day; `revoked` is true for a revoked device.
- `project` is normalised as credited minutes store it (control characters removed, surrounding space trimmed), and must then be 1–256 bytes.
- Optional query `device`: the agent's own device id. `this_device` is true for that device; it only helps the agent print which minutes it can back. Naming another user's device reveals nothing: every read is the caller's.
- `400` when `to` is after the `sealed_through` of some unrevoked device with rows in the range, saying the day the range can end on (the earliest `sealed_through` of those devices), or when such a device has sealed no day yet. Every unrevoked device with rows counts, whatever its rows' labels: their idle and cut rows are re-checked too. A revoked device can't seal any more days, so it never holds a range back: its rows on days it didn't seal are frozen and stay `unbacked`. The message says so, since a device that was replaced (a reinstall, a lost laptop) seals nothing more until the user revokes it.
- Optional query `submission`: the id of one of the caller's submissions. The plan is then that submission's **frozen** rows (every device) and the devices that have them, so a part built from it (`logbook submit … --add <id>`) lists no row the server didn't freeze. `project`, `from` and `to` must be the submission's (`400` naming them otherwise); the range isn't checked against sealed days (the freeze did that). `404` when it isn't the caller's submission (or isn't a UUID), and `409` as `POST /api/v1/submissions/{id}/parts` gives it when the submission takes no more parts.

## `POST /api/v1/submissions`

Auth: API key. `Content-Encoding: gzip` required; at most 32 MiB compressed and 256 MiB decompressed.

```json
{
  "project": "terra-bot",
  "from": "2026-11-01", "to": "2026-11-20",
  "program": "terra",
  "part": {
    "device_id": "<uuid>",
    "blocks": [
      {"day": "2026-11-03", "buckets": 812,
       "opened": [{"i": 0, "key": 0, "salt": "<hex>", "lines": ["{\"s\":48001,\"t\":\"…\",\"type\":\"repo\",\"v\":{…}}"]}],
       "edges": [{"i": 140, "key": 29361719, "bucket_hash": "<hex>"}],
       "hashes": ["<hex>", "…"]}
    ],
    "minutes": [{"minute": "2026-11-03T14:21:00Z", "computed_at": "…", "snapshot": 48213}]
  }
}
```
- `submission.Upload` and `submission.Part` in Go; `submission.Upload.Validate` is the synchronous shape check.
- `program` is optional: a public program's slug (ADR 0021). It is left out, not `""`, when there is none.
- `part.device_id` is a lowercase UUID.
- `blocks` follow ADR 0019 point 5: opened buckets with salt and lines, edge leaves, and subtree hashes. Each is its evidence `day` next to the proof's fields (`buckets`, `opened`, `edges`, `hashes`); at most one per day.
- `part.minutes` lists the minutes this part backs, each `{minute, computed_at, snapshot}`: whole UTC minutes, at most once each, with `minute` in `[computed_at − 60 min, computed_at)` and `snapshot ≥ 0`. Each must be a frozen row of that device with exactly that key, or the part is malformed. The server freezes **all** rows from the plan (every device) and the project's capped seconds per minute when it creates the submission; a minute not listed in any part is `unbacked`.
- The server checks synchronously, in this order: the body (gzip, size, JSON), `Upload.Validate`, the project, the range, the part's device (one of the caller's devices, revoked ones included: their evidence was signed before revocation), that every block's `day` has a stored evidence record with block fields, the program (a live public app), and the Hackatime trust gate. Then, in one repeatable-read transaction, it checks the range against every device with rows (as the plan does), freezes the rows, the capped seconds and the cap inputs, stores the part, writes the audit entry and queues the re-check. Proofs themselves are checked by the job.
- **Trust gate (ADR 0020):** a linked account's stored trust level decides. When it was last read more than 24 hours ago (or never), it is re-read from Hackatime first (the trust refresher, with a 10-second timeout, keeping `red` sticky); if Hackatime can't be reached the last known level decides. `red` is refused; every other level, and no link at all, may submit. The unmasked level is stored with the submission (`trust_at_submission`, `null` without a link).
- The body may take up to 10 minutes to arrive (the server's usual 30-second read timeout is extended for uploads).
- **Upload memory:** decoding a body holds several times its decompressed size (about 1.3 GB at the 256 MiB limit), so uploads (this route and `/parts`) pass a gate: one upload per user at a time (another while one runs is `429` with `Retry-After: 60`); the compressed body is read in full first; then it waits for one of 2 decode slots server-wide (`HandlerOptions.UploadSlots`), and after a minute (`UploadWait`) it is refused with `503` and `Retry-After: 60`. A malformed body costs the same until it is decoded.

| Status | Body | When |
|---|---|---|
| `202` | `{"id": "<uuid>", "status": "verifying", "review_url": "https://logbook.boogles.dev/r/<token>"}` | Accepted. Audited as `submission.create` (project, range, program, device and trust level; never the token). |
| `400` | error | Bad JSON or gzip, data after the JSON value, a shape error (`Upload.Validate`), a range ending after a device's sealed day, an unknown device, or a block with no stored evidence record. |
| `403` | `{"error": "your Hackatime account is banned, so you can't submit hours"}` | Linked Hackatime account is `red` (ADR 0020). |
| `404` | `{"error": "program not found"}` | Unknown, deleted or non-public program. |
| `409` | `{"error": "no counted minutes in this range"}` | No row of the project in the range has credit. |
| `413` | `{"error": "request body too large"}` | Over 32 MiB as sent or 256 MiB decompressed, found while the body streams. |
| `415` | error | No `Content-Encoding: gzip`. |
| `429` | error | The upload rate limit, or another upload of the user's is still in progress. |
| `503` | error | Every decode slot stayed busy for a minute. |

`review_url` is on the web UI's origin. The token is 128 random bits, base64url without padding (22 characters). It is returned in full here, to the submitter on `GET /api/v1/me/submissions*`, and to the program (its members' inbox and its client-credentials token), which pastes it into Airtable and decides through it. Once withdrawn, `review_url` is `null` everywhere.

## `POST /api/v1/submissions/{id}/parts`

Auth: API key. Same body as `part` above, from another device of the same user, gzip as above. The same synchronous checks apply (device, blocks, trust gate, the upload gate), and one more: every minute the part lists must be a frozen row of its device in this submission with the same key (`submission.CheckListed`, the re-check's own rule), or it is refused with `400` before it is stored. Such a part would otherwise fail the whole submission in the re-check, other devices' verified minutes included. The part is stored, the status goes back to `verifying`, `version` goes up by one, and the re-check runs again for the whole submission.

| Status | Body | When |
|---|---|---|
| `202` | `{"id": "<uuid>", "status": "verifying"}` | Accepted. |
| `400` | error | As above, or the part lists a minute that isn't a frozen row of its device with that key. |
| `403`, `413`, `415`, `429`, `503` | error | As above. |
| `404` | `{"error": "submission not found"}` | Not one of the caller's submissions. |
| `409` | error | A decision exists, the submission was withdrawn or failed (submit again), its stored parts were deleted by retention (the re-check of the whole submission needs them; submit again), or that device already has a part. |

The agent (`logbook submit … --add <id>`) accepts `200`, `201`, `202` or `204`, with the body of `POST /api/v1/submissions` or none, and sends the same `Content-Encoding: gzip` and size limits.

## The agent's side

[docs/agent/submit.md](../agent/submit.md) settles what `logbook submit` and `logbook verify` do with these endpoints: the agent lists in its part only minutes whose rows share one key whose window holds the minute and that `crediting.BuildProofs` backs, and with `--add` only the submission's frozen rows (its plan with `submission=<id>`), so a part it builds never fails the submission; prints which other projects' rows and repo folders the part reveals before asking, refuses a range ending today, and maps `401`/`403`/`400`/`404`/`409`/`413`/`429` to plain messages. It never retries an upload by itself.

## `GET /api/v1/me/submissions`

Auth: session. `{"submissions": [...]}`, the user's submissions, newest first, each:

```json
{"id": "…", "project": "terra-bot", "from": "2026-11-01", "to": "2026-11-20",
 "program": {"slug": "terra", "name": "Terra"}, "status": "ready", "error": "…",
 "counted_seconds": 44640, "raw_seconds": 61200, "minutes": {"verified": 744, "mismatch": 0, "unbacked": 0},
 "decision": {"value": "adjusted", "approved_seconds": 40000, "reason": "…", "by": "reviewer-username", "at": "…"},
 "review_url": "https://logbook.boogles.dev/r/<token>", "created_at": "…"}
```
- `program` is `null` without one. A deleted program keeps its slug and name.
- `error` is only present on `failed`; it says which device's part failed and why.
- `counted_seconds`, `raw_seconds` and `minutes` are 0 until the submission is `ready`.
- `decision` is the latest, or `null`. `approved_seconds` is `null` except for `adjusted`; `by` is `null` once the reviewer's account is gone.
- `review_url` is `null` once withdrawn.

## `GET /api/v1/me/submissions/{id}`

Auth: session. The same object plus `packet` (below), `null` unless the submission is `ready`.

## `DELETE /api/v1/me/submissions/{id}`

Auth: session + CSRF. Withdraws: status becomes `withdrawn`, the review link answers `410`, the stored parts (the raw rows) are deleted, and the packet, frozen rows and counts stay. `204`; withdrawing again is a no-op `204`. `409 {"error": "a decision has been recorded; it can't be withdrawn"}` once decided. Audited as `submission.withdraw`. A re-check already running when it is withdrawn doesn't write its result.

## `GET /api/v1/review/{token}`

Auth: none (the token is the credential). `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `X-Robots-Tag: noindex`.

`200`:
```json
{
  "submission": {"id": "…", "project": "terra-bot", "from": "…", "to": "…", "status": "ready",
                 "user": {"username": "fiona", "display_name": "Fiona", "hackatime_id": 4321},
                 "program": {"slug": "terra", "name": "Terra"}, "created_at": "…", "version": 1},
  "packet": {
    "rules_version": "v1",
    "counted_seconds": 44640,
    "trust": {"at_submission": "green", "now": "green", "checked_at": "…", "hackatime_linked": true,
              "hackatime_linked_now": true, "hackatime_account_changed": false},
    "raw_seconds": 61200,
    "raw_ai_seconds": 9000,
    "one_third_rule_seconds": 55200,
    "minutes": {"verified": 744, "mismatch": 0, "unbacked": 0},
    "devices": [{"name": "laptop", "os": "windows", "gaps": ["2026-11-07"]}],
    "made_of": {"editing": 21000, "with_ai": 17640, "other": 6000},
    "cuts": [{"code": "idle", "rule": "Nobody was at the keyboard and no AI agent was working", "seconds": 9000,
              "ranges": [["2026-11-03T15:02:00Z", "2026-11-03T15:20:00Z"]]}],
    "flags": [{"code": "low_signal", "rule": "Credited without an OS input signal, from editor activity alone", "minutes": 12,
               "ranges": [["…", "…"]]}],
    "days": [{"day": "2026-11-03", "strip": "yyyyaaaii…", "counted_seconds": 14400, "raw_seconds": 18000,
              "commits": [{"at": "…", "hash": "1a2b3c4", "files": 3, "added": 120, "removed": 14, "url": "https://github.com/…/commit/…"}]}],
    "ai_sessions": [{"harness": "claude-code", "model": "…", "from": "…", "to": "…", "prompts": 14, "turns": 14, "counted_seconds": 3300, "agent_alone_seconds": 600}],
    "mismatches": [{"minute": "…", "device": {"label": "you", "credited": 60, "cuts": []}, "server": {"label": "idle", "credited": 0, "cuts": ["idle"]}}],
    "justification": "Tracked with Logbook (rules v1). 12.4 h counted of 17.0 h at the keyboard …",
    "raw_rows_deleted_after": "2027-11-25"
  },
  "decision": {"value": "adjusted", "approved_seconds": 40000, "reason": "…", "by": "reviewer-username", "at": "…"}
}
```
The packet is `submission.Packet`, built by `submission.BuildPacket` (pure, so re-running the job gives the same packet). Field by field:
- `strip`: one character per minute of the UTC day, for every day of the range: `y` counted, `a` AI ran alone, `c` cut (counted label but 0 credit), `i` idle or nothing, `u` counted but unbacked, `m` mismatch. Precedence: `m` when any of the minute's frozen rows mismatches; else, when the minute is counted (capped seconds above 0, or a row with credit), `u` if a row with credit is unbacked and `y` otherwise; else `c` for a `you` row with no credit and a cut; else `a` for an `agent` row; else `i`.
- `counted_seconds` and everything derived from it use the frozen **capped** seconds (ADR 0020). Each day also has its own `counted_seconds` and `raw_seconds`. `trust` levels are masked as Hackatime masks them publicly (`yellow` and `unknown` show as `blue`). `trust.now`, `checked_at` and `hackatime_linked_now` are read when the response is made; the stored packet has only `at_submission` and `hackatime_linked` (whether Hackatime was linked at submission). A submitter who has unlinked since shows `now` as `blue` (as any unlinked user) with `hackatime_linked_now: false`. The submission records the Hackatime account (its user id) linked when it was made; when Hackatime was linked then, `hackatime_account_changed` is set when served: `true` when the submitter is linked now to another Hackatime account, so `now` is that other account's level, not the level of the account they submitted under. It is absent when Hackatime wasn't linked at submission.
- `rules_version` is the frozen rows' rules version (several are joined with `, `).
- `minutes` counts distinct submitted minutes, every label (cut and idle minutes too, since the re-check verifies cuts as well): a minute is `mismatch` when any of its rows (one per device) mismatches, else `unbacked` when any is unbacked, else `verified`.
- `devices` are the devices with frozen rows. `gaps` are the range's days, from the device's first sealed day on, that it has no evidence record for (every day when it has sealed none).
- `justification` is built when the response is made (it contains the link) by `submission.Justification`; it isn't stored. Its form: "Tracked with Logbook (rules v1). 12.4 h counted of 17.0 h at the keyboard; the one-third AI rule would give 15.3 h. All 2,431 minutes re-computed on our server from signed device logs. Review: <link>", or, with mismatched or unbacked minutes, "1,000 of 2,236 minutes re-computed on our server from signed device logs; 2 differ from the device's result and 1,234 are not yet backed by evidence."
- `raw_rows_deleted_after` is also set when served.
- `one_third_rule_seconds` = `raw_seconds − raw_ai_seconds + raw_ai_seconds / 3`, rounded down. `raw_*` come from heartbeats for the same project and days, the same way `/api/v1/me/summary` counts time at the keyboard.
- `made_of` splits `counted_seconds` by the revealed rows of each counted minute: `editing` (a human heartbeat for the project in the minute), else `with_ai` (an AI event of the project in the minute, such as a prompt, a permission answer or the agent working while the person watches, or a turn of a session of the project open into it: its last event before the minute is under 10 minutes earlier and isn't `turn_end` or `session_end`), else `other`. Each device row reads only its own revealed rows with `seq ≤ H`, attributed to projects exactly as the engine does (`struggle.RepoMap` at `H`). A minute with rows on several devices takes the best of them; unbacked rows reveal nothing, so a minute with only unbacked rows goes to `other`.
- `cuts` and `flags` summarise the frozen rows' codes (the device's values, mismatched rows included), each with its rule in plain words (`submission.CutRules`, `submission.FlagRules`) and its minutes merged into `[start, end)` ranges. A cut's `seconds` is 60 per minute it marks. `multi_device` (ADR 0014) is added from the frozen cap inputs: submitted minutes in which two or more devices credited time, to any project.
- `commits` are this project's git commit events (by the event's own project name) with a hash of 7 to 64 lowercase hex digits, from the revealed rows, once per hash (the earliest), by time; `hash` is the first 7 characters. `url` only when the project's `repo_url` is `https://github.com/<owner>/<repo>` (optionally `.git` or a trailing slash, no query).
- `ai_sessions` are the project's AI sessions with an event in the range, from each device's revealed AI events attributed with the mapping at the device's highest backed key, sorted by start. `prompts` counts `prompt_submit` events and `turns` `turn_end` events; `model` is the latest named. A session's minutes are the minutes of its events, plus every minute from an event that doesn't end a turn to the session's next event when that is at most 10 minutes later; `counted_seconds` sums their capped seconds and `agent_alone_seconds` is 60 per one of them with an `agent` row.
- `mismatches` lists at most 200, newest first (ties by device). `server` is the replay's row for the project, or `{"label": "idle", "credited": 0, "cuts": []}` when the replay has none.
- Never in the packet: file paths, other projects, prompt hashes, session ids, device ids.

The `submission` object: `version` identifies the packet: it starts at 1 and goes up by one with each part added (a re-check of the same version gives the same packet). `user.hackatime_id` is `null` when Hackatime isn't linked; `program` is `null` without one (a deleted program keeps its slug and name). It never carries the failure reason, the trust level unmasked or a device id.

| Status | When |
|---|---|
| `200` | `ready`: `submission`, `packet` and `decision` (`null` when none). `failed`: `submission` (status `failed`) with `packet` and `decision` `null`; the reason is shown only to the submitter. |
| `202` | Still `verifying` (body has `submission` only). |
| `404` | Unknown or malformed token. |
| `410` | Withdrawn. |

Every answer, errors and `429` included, carries the three headers above.

## `POST /api/v1/review/{token}/decision`

Auth: session + CSRF; the caller must be a member (owner or reviewer, ADR 0021) of the submission's program while the app is live, and not its submitter. Membership is the `oauth` package's (`GetApp`).

```json
{"value": "adjusted", "approved_seconds": 40000, "reason": "Two days of idle editor time", "version": 1}
```
- `approved`: no other field (`approved_seconds` or a non-blank `reason` is `400`). `adjusted`: `approved_seconds` from 0 to below the frozen `counted_seconds`, and a `reason`. `rejected`: a `reason`, no `approved_seconds`. A reason is trimmed, then 1–500 characters.
- `version` is optional: the review's `submission.version` the reviewer read. When given and a part has been added since, the decision is `409` ("reload it"). The web UI sends it. Without it the decision applies to the packet as it is when recorded.
- Before recording, the submitter's trust level is re-read from Hackatime when it is more than 24 hours old (the last known level decides if Hackatime can't be reached); approving or adjusting while it is `red` is refused, rejecting is allowed. Approving or adjusting is also refused when the submitter was linked at submission and isn't linked to that same Hackatime account now: they unlinked it, or linked another account (`hackatime_account_changed`). Either way a ban of the account they submitted under could no longer be checked: unlinking or switching accounts is refused only for a stored `red`, which can be up to a day old. Linking the original account again restores it. A submitter who wasn't linked at submission has no account to compare, so linking one later changes nothing here.
- Adds a decision; the newest is the one shown. Every decision is kept (`submission_decisions`) and audited (`submission.decide`, by the reviewer, with the value, approved seconds, reason, counted seconds, program and the submitter's trust level).
- Raw rows are then kept for a year after the newest decision.

| Status | When |
|---|---|
| `200` | Recorded; `{"decision": {"value", "approved_seconds", "reason", "by", "at"}}`. |
| `400` | The body breaks the rules above. |
| `403` | Not a member, the submitter, a submission with no program (it takes no decisions), or a deleted program. |
| `404` | Unknown token. |
| `409` | Status isn't `ready`, the given `version` is no longer current, or approving/adjusting while the submitter's Hackatime account is banned now, or isn't the account they submitted under (unlinked or switched since submitting). |
| `410` | Withdrawn. |

## The program's inbox

The submissions addressed to a program, newest first, as in `GET /api/v1/me/submissions` plus `user` (`{username, display_name, hackatime_id}`) and without `error`. Query: `status` (`verifying`, `ready`, `failed`, `withdrawn`), `decision` (`approved`, `adjusted`, `rejected`, or `none` for no decision yet), `limit` (1–500, default 100) and `offset` (0–1,000,000); anything else is `400`.

| Call | Auth | What |
|---|---|---|
| `GET /api/v1/me/apps/{id}/submissions` | session; a member (owner or reviewer) of the live app | `{"submissions": [...]}`. `404 {"error": "not found"}` for anyone else, as on every `/api/v1/me/apps/{id}…` call |
| `GET /api/v1/submissions` | the app's own client-credentials token with scope `submissions` (`oauth.RequireScope`) | `{"submissions": [...]}` of that app only |
| `GET /api/v1/submissions/{id}` | as above | One submission with `packet` (as served on the review link, justification included; `null` unless `ready`). `404` unless it is addressed to the token's app |

Session cookies, API keys and user-granted tokens never pass the client-credentials routes (`401`, or `403 insufficient_scope` for a user's token), and a client token never passes the session routes.

## The re-check job (River, queue `submissions`)

For each part, `submission.VerifyPart(part, device, frozen rows of the device)`:
1. Load **every** stored `evidence_days` record of the part's device that has block fields (ADR 0019), with its chain hash and signature, and the device's public key. Each record's signature must verify and each must link to the one before it (`prev_chain_hash` is its chain hash and `seq_from` its `seq_to`). Every block in the part must match one record, by day; verify its proof against `content_hash` (ADR 0019).
2. Decode the opened lines with `pkg/evidence`. Reject the part if a line doesn't decode, its `seq` is outside the block's `(seq_from, seq_to]`, a repo row is outside the repo bucket (key `0`) or another row isn't in the minute bucket of its time, or sequence numbers don't increase within a bucket. Every minute in `part.minutes` must be a frozen row of the device with the same key.
3. Group the part's minutes by key. A minute is `unbacked` unless every block whose signed time span overlaps its window `[T − 120 min, T)` and whose `seq_from` is below `H` proves completeness for that window, and the repo bucket of `H`'s block is opened. For each key and rules version, build the engine input with `pkg/replay` from the opened minute rows with `seq ≤ H` in the window and the repo rows with `seq ≤ H` of `H`'s block's repo bucket (reduced to the highest `seq` per path), and run `pkg/struggle` at the minute's recorded rules version, once per key. Compare label, project, credit and cuts with the frozen rows (`replay.Same`; flags are advisory).
4. Mark each frozen row `verified` or `mismatch`; rows of devices with no part stay `unbacked`.

An unbacked row carries why (`submission.Reason*`): no part from its device yet; stored before computation keys; not listed in its device's part; no sealed block holds `H`; the repo bucket of `H`'s block isn't opened; its window isn't proven complete; or the chain's earliest record links to an earlier one and starts above sequence 0 (it follows a day stored before ADR 0019, so rows the run read may be in no record). A device's first record may start above 0 with no previous chain hash: that is a takeover baseline (ADR 0019 point 4), and rows below it aren't required.

Then check the cap (`submission.CheckCap`), build the packet and set `ready`. A part that fails step 1 or 2 (a bad signature or chain, a proof that doesn't verify, a listed minute that isn't frozen with that key), or a rules version the server can't run, sets the submission to `failed` with the reason; it never makes minutes unbacked. The job is idempotent: re-running it gives the same packet.

How the server runs it (`submissions.Checker.Verify`, job kind `submission_verify`, up to 10 attempts, 15 minutes each):
- It works only on a `verifying` submission. Devices are those with frozen rows or a part, by id; each one's records come from the ledger (`ledger.Service.Days`, every stored day: records with block fields go to the verifier, every day counts for the packet's `gaps`).
- The packet's inputs: the frozen capped seconds and cap inputs, the revealed rows of each part, raw seconds per UTC day of the project from `user_minutes` (label `you`, and its `ai coding` category for `raw_ai_seconds`), the project's `repo_url`, and the trust level stored at submission, masked.
- A `failed` reason names the device by its name and says what failed. Database or ledger errors are retried by River instead.
- Each part added bumps the submission's `parts_version`. A run writes its result (every frozen row's `result`, `reason` and the server's row on a mismatch; the packet; the counts) only while the submission is still `verifying` at the version it read, so a run overtaken by a newer part or by a withdrawal changes nothing; the newer part's own job writes. Jobs aren't unique, so two runs of one version write the same result.

## Retention

A periodic job (`submission_raw_retention`, every 6 hours, queue `submissions`) deletes the stored parts (the revealed raw rows) of submissions whose newest decision, or creation when there is none, is more than a year old: `data` becomes `null` and `raw_deleted_at` is set. The packet, frozen rows, cap inputs and decisions stay while the account exists. Withdrawal deletes the parts at once.

## The verification bundle

`GET /api/admin/v1/submissions/{id}/bundle` (ADR 0022) exports, gzip-compressed, the JSON that `logbook verify` checks with `submission.CheckBundle` (`submission.Bundle` in Go):

```json
{
  "format": "logbook-submission-bundle-v1",
  "submission": {"id": "<uuid>", "project": "terra-bot", "from": "2026-11-01", "to": "2026-11-20", "created_at": "…"},
  "devices": [
    {"id": "<uuid>", "name": "laptop", "public_key": "<standard base64 of the 32-byte key>",
     "evidence_days": [{"day": "2026-11-03", "prev_chain_hash": "…", "content_hash": "…", "chain_hash": "…",
                        "signature": "<standard base64>", "rules_version": "v1", "event_counts": {"activity": 9120},
                        "seq_from": 48001, "seq_to": 58075, "buckets": 612,
                        "time_from": "2026-11-03T07:58:00Z", "time_to": "2026-11-03T22:41:00Z"}]}
  ],
  "parts": [{"device_id": "<uuid>", "blocks": ["…"], "minutes": ["…"]}],
  "minutes": [{"device_id": "<uuid>", "minute": "…", "project": "terra-bot", "label": "you", "credited": 60,
               "cuts": [], "flags": [], "rules_version": "v1", "computed_at": "…", "snapshot": 48213}],
  "capped": [{"minute": "…", "seconds": 60}],
  "cap_inputs": [{"device_id": "<uuid>", "minute": "…", "project": "terra-bot", "credited": 60}]
}
```
- `devices` are every device with a frozen row or a part. `evidence_days` are each device's every stored record with block fields, in the JSON of `POST /api/v1/devices/{id}/evidence-days` plus the stored chain hash and signature.
- `parts` are the parts exactly as uploaded (at most one per device); `minutes` the frozen rows; `capped` the frozen capped seconds, one per minute of the frozen rows; `cap_inputs` every `credited_minutes` row of the user at those minutes, any project, any device, whatever its credit.
- The server builds it with `submissions.Checker.Bundle` from what it stored: the frozen rows, capped seconds and cap inputs as frozen, the stored parts (decoded from their gzip), and each device's records and key read as the re-check reads them. It checks no caller; the admin route must. Once the parts are deleted (withdrawal or retention) it fails with `submissions.ErrEvidenceDeleted`, which the route answers `410`. The end-to-end test (`internal/e2e`) runs `logbook verify`'s check on it and gets the packet's counts.
- `CheckBundle` verifies each part as the job does, marks rows of devices with no part `unbacked`, and checks that the frozen rows are among the cap inputs, that `capped` is exactly the cap recomputed from them (`submission.Cap`, the `credited_project_minutes` rule: a project's credit summed over devices, and when the minute's total over every project and device is above 60, `credit × 60 / total` rounded down) and that every capped minute has a frozen row. It reports the packet's `minutes` counts and `counted_seconds`.

## Storage

Migration `00008_submissions.sql`:
- `submissions`: id, user, project, range (`from_day`, `to_day`), program (`program_app_id`, set to `null` only if the app's row is ever removed), status, `error`, token SHA-256 (unique, the lookup) and AES-256-GCM token (the server key, the submission id as associated data), unmasked trust level at submission (`null` without a link), packet (`jsonb`, no link in it), minute counts, `counted_seconds`, `raw_seconds`, `parts_version`, and the created, verified, latest decision and withdrawn times. Indexed by (user, created) and (program, created).
- `submission_minutes`: the frozen device rows (device, minute, label, credit, cuts, flags, rules version, key; the project is the submission's), with `result` (`verified`, `mismatch`, `unbacked`, `null` while verifying), `reason` for unbacked rows and the server's label, credit and cuts on a mismatch.
- `submission_capped`: the frozen capped seconds per minute for the project, one per minute of the frozen rows (0 included), as `credited_project_minutes` gives them.
- `submission_cap_inputs`: every `credited_minutes` row of the user (any project, any device, whatever its credit) at the submitted minutes, frozen, so the cap can be recomputed offline.
- `submission_parts`: device (one part per device), the part's JSON gzip-compressed (`bytea`, `null` once the raw rows are deleted), its compressed size, uploaded time, raw rows deleted time.
- `submission_decisions`: every decision (value, approved seconds, reason, reviewer, time), newest wins.

The freeze is one repeatable-read transaction: the rows, cap inputs and capped seconds come from one snapshot, together with the range check, the stored part, the audit entry and the queued job.
