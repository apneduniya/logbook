# Submissions and review API

How a user submits a project's hours, how the server re-checks them, and what reviewers read (ADR 0019, ADR 0020). Packages: `internal/server/submissions` (upload, storage, the verification job, serving the packet), `pkg/submission` (the wire types below, the verifier, the cap, the packet builder and the offline bundle check, shared with the agent and `logbook verify`), `pkg/replay`, `pkg/evidence`.

## Conventions

As in [devices.md](devices.md): API-key auth for the agent, session auth (plus `X-CSRF-Token` on writes) for the browser, errors as `{"error": "message"}`. Times are RFC 3339 UTC; days are `YYYY-MM-DD` UTC.

- A submission `{id}` that isn't a UUID, doesn't exist, or isn't the caller's is `404 {"error": "submission not found"}`.
- **Rate limits** per user: `POST /api/v1/submissions` and `POST /api/v1/submissions/{id}/parts` share a burst of 10, refilled at 10 per hour. `GET /api/v1/review/{token}`: per IP, a burst of 120, refilled at 120 per minute.

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
    {"id": "<uuid>", "name": "laptop", "sealed_through": "2026-11-24", "this_device": true}
  ],
  "minutes": [
    {"device_id": "<uuid>", "minute": "2026-11-03T14:21:00Z", "project": "terra-bot",
     "label": "you", "credited": 60, "cuts": [], "flags": [], "rules_version": "v1",
     "computed_at": "2026-11-03T14:30:00Z", "snapshot": 48213}
  ]
}
```
- `submission.Plan` in Go. `minutes` are the rows the server holds for this project and range, every device and every label (cut and idle rows too), sorted by (device, minute). Rows stored before ADR 0017 have no key: `computed_at` and `snapshot` are `null`, and they can't be backed.
- `sealed_through` is `null` for a device that has sealed no day.
- Optional query `device`: the agent's own device id. `this_device` is true for that device; it only helps the agent print which minutes it can back.
- `400` when `to` is not before every included device's `sealed_through + 1 day`, with the day it can end on.

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
- The server checks the shape synchronously and queues the re-check.

| Status | Body | When |
|---|---|---|
| `202` | `{"id": "<uuid>", "status": "verifying", "review_url": "https://logbook.boogles.dev/r/<token>"}` | Accepted. Audited as `submission.create`. |
| `400` | error | Bad JSON, range, unknown device, block with no stored evidence day, or a proof that doesn't parse. |
| `403` | `{"error": "your Hackatime account is banned, so you can't submit hours"}` | Linked Hackatime account is `red` (ADR 0020). |
| `404` | `{"error": "program not found"}` | Unknown or non-public program. |
| `409` | `{"error": "no counted minutes in this range"}` | Nothing to submit. |
| `413` | error | Too large. |

`review_url` is the only time the token is returned in full besides `GET /api/v1/me/submissions/{id}`.

## `POST /api/v1/submissions/{id}/parts`

Auth: API key. Same body as `part` above, from another device of the same user. Re-runs the check for the whole submission. `409` once a decision exists or when that device already has a part.

## `GET /api/v1/me/submissions`

Auth: session. The user's submissions, newest first: `id`, `project`, `from`, `to`, `program` (`{slug, name}` or `null`), `status`, `counted_seconds`, `raw_seconds`, `minutes` (`{verified, mismatch, unbacked}`), `decision`, `review_url`, `created_at`.

## `GET /api/v1/me/submissions/{id}`

Auth: session. The same object plus `packet` (below).

## `DELETE /api/v1/me/submissions/{id}`

Auth: session + CSRF. Withdraws: status becomes `withdrawn`, the review link answers `410`, raw rows are deleted. `409 {"error": "a decision has been recorded; it can't be withdrawn"}` once decided. Audited as `submission.withdraw`.

## `GET /api/v1/review/{token}`

Auth: none (the token is the credential). `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `X-Robots-Tag: noindex`.

`200`:
```json
{
  "submission": {"id": "…", "project": "terra-bot", "from": "…", "to": "…", "status": "ready",
                 "user": {"username": "fiona", "display_name": "Fiona", "hackatime_id": 4321},
                 "program": {"slug": "terra", "name": "Terra"}, "created_at": "…"},
  "packet": {
    "rules_version": "v1",
    "counted_seconds": 44640,
    "trust": {"at_submission": "green", "now": "green", "checked_at": "…", "hackatime_linked": true},
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
- `counted_seconds` and everything derived from it use the frozen **capped** seconds (ADR 0020). Each day also has its own `counted_seconds` and `raw_seconds`. `trust` levels are masked as Hackatime masks them publicly (`yellow` and `unknown` show as `blue`). `trust.now` and `checked_at` are read when the response is made; the stored packet has only `at_submission` and `hackatime_linked`.
- `rules_version` is the frozen rows' rules version (several are joined with `, `).
- `minutes` counts distinct submitted minutes, every label (cut and idle minutes too, since the re-check verifies cuts as well): a minute is `mismatch` when any of its rows (one per device) mismatches, else `unbacked` when any is unbacked, else `verified`.
- `devices` are the devices with frozen rows. `gaps` are the range's days, from the device's first sealed day on, that it has no evidence record for (every day when it has sealed none).
- `justification` is built when the response is made (it contains the link) by `submission.Justification`; it isn't stored. Its form: "Tracked with Logbook (rules v1). 12.4 h counted of 17.0 h at the keyboard; the one-third AI rule would give 15.3 h. All 2,431 minutes re-computed on our server from signed device logs. Review: <link>", or, with mismatched or unbacked minutes, "1,000 of 2,236 minutes re-computed on our server from signed device logs; 2 differ from the device's result and 1,234 are not yet backed by evidence."
- `raw_rows_deleted_after` is also set when served.
- `one_third_rule_seconds` = `raw_seconds − raw_ai_seconds + raw_ai_seconds / 3`, rounded down. `raw_*` come from heartbeats for the same project and days, the same way `/api/v1/me/summary` counts time at the keyboard.
- `made_of` splits `counted_seconds` by the revealed rows of each counted minute: `editing` (a human heartbeat for the project in the minute), else `with_ai` (an AI event of the project in the minute, such as a prompt, a permission answer or the agent working while the person watches, or a turn of a session of the project open into it: its last event before the minute is under 10 minutes earlier and isn't `turn_end` or `session_end`), else `other`. Each device row reads only its own revealed rows with `seq ≤ H`, attributed to projects exactly as the engine does (`struggle.RepoMap` at `H`). A minute with rows on several devices takes the best of them; unbacked rows reveal nothing, so a minute with only unbacked rows goes to `other`.
- `cuts` and `flags` summarise the frozen rows' codes (the device's values, mismatched rows included), each with its rule in plain words (`submission.CutRules`, `submission.FlagRules`) and its minutes merged into `[start, end)` ranges. A cut's `seconds` is 60 per minute it marks. `multi_device` (ADR 0014) is added from the frozen cap inputs: submitted minutes in which two or more devices credited time, to any project.
- `commits` are this project's git commit events (by the event's own project name) from the revealed rows, once per hash (the earliest), by time; `hash` is the first 7 characters. `url` only when the project's `repo_url` is `https://github.com/<owner>/<repo>` (optionally `.git` or a trailing slash, no query).
- `ai_sessions` are the project's AI sessions with an event in the range, from each device's revealed AI events attributed with the mapping at the device's highest backed key, sorted by start. `prompts` counts `prompt_submit` events and `turns` `turn_end` events; `model` is the latest named. A session's minutes are the minutes of its events, plus every minute from an event that doesn't end a turn to the session's next event when that is at most 10 minutes later; `counted_seconds` sums their capped seconds and `agent_alone_seconds` is 60 per one of them with an `agent` row.
- `mismatches` lists at most 200, newest first (ties by device). `server` is the replay's row for the project, or `{"label": "idle", "credited": 0, "cuts": []}` when the replay has none.
- Never in the packet: file paths, other projects, prompt hashes, session ids, device ids.

| Status | When |
|---|---|
| `404` | Unknown token. |
| `410` | Withdrawn. |
| `202` | Still `verifying` (body has `submission` only). |

## `POST /api/v1/review/{token}/decision`

Auth: session + CSRF; the caller must be a member of the submission's program (ADR 0021) and not its submitter.

```json
{"value": "adjusted", "approved_seconds": 40000, "reason": "Two days of idle editor time"}
```
- `approved`: no other field. `adjusted`: `approved_seconds` below `counted_seconds`, and a `reason` (1–500 characters). `rejected`: a `reason`.
- Replaces the current decision; every decision is kept in `audit_log` (`submission.decide`).

| Status | When |
|---|---|
| `200` | Recorded; returns `decision`. |
| `403` | Not a member, or the submitter. |
| `409` | Status isn't `ready`, or approving/adjusting while the submitter's Hackatime account is banned now. |

## The re-check job (River, queue `submissions`)

For each part, `submission.VerifyPart(part, device, frozen rows of the device)`:
1. Load **every** stored `evidence_days` record of the part's device that has block fields (ADR 0019), with its chain hash and signature, and the device's public key. Each record's signature must verify and each must link to the one before it (`prev_chain_hash` is its chain hash and `seq_from` its `seq_to`). Every block in the part must match one record, by day; verify its proof against `content_hash` (ADR 0019).
2. Decode the opened lines with `pkg/evidence`. Reject the part if a line doesn't decode, its `seq` is outside the block's `(seq_from, seq_to]`, a repo row is outside the repo bucket (key `0`) or another row isn't in the minute bucket of its time, or sequence numbers don't increase within a bucket. Every minute in `part.minutes` must be a frozen row of the device with the same key.
3. Group the part's minutes by key. A minute is `unbacked` unless every block whose signed time span overlaps its window `[T − 120 min, T)` and whose `seq_from` is below `H` proves completeness for that window, and the repo bucket of `H`'s block is opened. For each key and rules version, build the engine input with `pkg/replay` from the opened minute rows with `seq ≤ H` in the window and the repo rows with `seq ≤ H` of `H`'s block's repo bucket (reduced to the highest `seq` per path), and run `pkg/struggle` at the minute's recorded rules version, once per key. Compare label, project, credit and cuts with the frozen rows (`replay.Same`; flags are advisory).
4. Mark each frozen row `verified` or `mismatch`; rows of devices with no part stay `unbacked`.

An unbacked row carries why (`submission.Reason*`): no part from its device yet; stored before computation keys; not listed in its device's part; no sealed block holds `H`; the repo bucket of `H`'s block isn't opened; its window isn't proven complete; or the chain's earliest record links to an earlier one and starts above sequence 0 (it follows a day stored before ADR 0019, so rows the run read may be in no record). A device's first record may start above 0 with no previous chain hash: that is a takeover baseline (ADR 0019 point 4), and rows below it aren't required.

Then check the cap (`submission.CheckCap`), build the packet and set `ready`. A part that fails step 1 or 2 (a bad signature or chain, a proof that doesn't verify, a listed minute that isn't frozen with that key), or a rules version the server can't run, sets the submission to `failed` with the reason; it never makes minutes unbacked. The job is idempotent: re-running it gives the same packet.

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
- `CheckBundle` verifies each part as the job does, marks rows of devices with no part `unbacked`, and checks that the frozen rows are among the cap inputs, that `capped` is exactly the cap recomputed from them (`submission.Cap`, the `credited_project_minutes` rule: a project's credit summed over devices, and when the minute's total over every project and device is above 60, `credit × 60 / total` rounded down) and that every capped minute has a frozen row. It reports the packet's `minutes` counts and `counted_seconds`.

## Storage

- `submissions`: id, user, project, range, program, status, token hash and encrypted token, trust level at submission, packet (`jsonb`, no link in it), counts, created and decided times.
- `submission_minutes`: the frozen device rows, with `result` (`verified`, `mismatch`, `unbacked`) and the server's result on mismatch.
- `submission_capped`: the frozen capped seconds per minute for the project, one per minute of the frozen rows (0 included), as `credited_project_minutes` gives them.
- `submission_cap_inputs`: every `credited_minutes` row of the user (any project, any device, whatever its credit) at the submitted minutes, frozen, so the cap can be recomputed offline.
- `submission_parts`: device, gzip bundle (`bytea`), uploaded time, raw rows deleted time.
- `submission_decisions`: every decision, newest wins.
