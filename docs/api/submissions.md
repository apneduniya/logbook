# Submissions and review API

How a user submits a project's hours, how the server re-checks them, and what reviewers read (ADR 0019, ADR 0020). Packages: `internal/server/submissions` (upload, verification job, packet), `pkg/replay`, `pkg/evidence`.

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
     "label": "you", "credited": 60, "cuts": [], "flags": [], "rules_version": "1",
     "computed_at": "2026-11-03T14:30:00Z", "snapshot": 48213}
  ]
}
```
- `minutes` are the rows the server holds for this project and range, every device, sorted by (device, minute). Rows stored before ADR 0017 have no key and can't be backed.
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
- `program` is optional: a public program's slug (ADR 0021).
- `blocks` follow ADR 0019 point 5: opened buckets with salt and lines, edge leaves, and subtree hashes.
- `part.minutes` lists the minutes this part backs, by key. The server freezes **all** rows from the plan (every device) and the project's capped seconds per minute when it creates the submission; a minute not listed in any part is `unbacked`.
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
    "rules_version": "1",
    "counted_seconds": 44640,
    "trust": {"at_submission": "green", "now": "green", "hackatime_linked": true},
    "raw_seconds": 61200,
    "raw_ai_seconds": 9000,
    "one_third_rule_seconds": 55200,
    "minutes": {"verified": 744, "mismatch": 0, "unbacked": 0},
    "devices": [{"name": "laptop", "os": "windows", "gaps": ["2026-11-07"]}],
    "made_of": {"editing": 21000, "with_ai": 17640, "other": 6000},
    "cuts": [{"code": "idle", "seconds": 9000, "ranges": [["2026-11-03T15:02:00Z", "2026-11-03T15:20:00Z"]]}],
    "flags": [{"code": "low_signal", "minutes": 12, "ranges": [["…", "…"]]}],
    "days": [{"day": "2026-11-03", "strip": "yyyyaaaii…", "commits": [{"at": "…", "hash": "1a2b3c4", "files": 3, "added": 120, "removed": 14, "url": "https://github.com/…/commit/…"}]}],
    "ai_sessions": [{"harness": "claude-code", "model": "…", "from": "…", "to": "…", "prompts": 14, "turns": 14, "counted_seconds": 3300, "agent_alone_seconds": 600}],
    "mismatches": [{"minute": "…", "device": {"label": "you", "credited": 60, "cuts": []}, "server": {"label": "idle", "credited": 0, "cuts": ["idle"]}}],
    "justification": "Tracked with Logbook (rules v1). 12.4 h counted of 17.0 h at the keyboard …",
    "raw_rows_deleted_after": "2027-11-25"
  },
  "decision": {"value": "adjusted", "approved_seconds": 40000, "reason": "…", "by": "reviewer-username", "at": "…"}
}
```
- `strip`: one character per minute of the UTC day: `y` counted, `a` AI ran alone, `c` cut (counted label but 0 credit), `i` idle or nothing, `u` counted but unbacked, `m` mismatch.
- `counted_seconds` and everything derived from it use the frozen **capped** seconds (ADR 0020). `trust` levels are masked as Hackatime masks them publicly (`yellow` shows as `blue`).
- `justification` is built when the response is made (it contains the link); it isn't stored.
- `one_third_rule_seconds` = `raw_seconds − raw_ai_seconds + raw_ai_seconds / 3`, rounded down. `raw_*` come from heartbeats for the same project and days, the same way `/api/v1/me/summary` counts time at the keyboard.
- `made_of` splits `counted_seconds` by the revealed rows of each counted minute: `editing` (a human heartbeat for the project), else `with_ai` (a prompt, a permission answer, or a turn of the person's AI session), else `other`. Unbacked minutes go to `other`.
- `commits` are this project's git events from the revealed rows. `url` only when the project's `repo_url` is a GitHub repository.
- `mismatches` lists at most 200, newest first.
- Never in the packet: file paths, other projects, prompt hashes, session ids.

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

For each part:
1. Load the stored `evidence_days` of the part's device. Every block in the part must match one, by day; verify its proof against `content_hash` (ADR 0019).
2. Decode the opened lines with `pkg/evidence`. Reject the part if a line doesn't decode, a line's minute doesn't match its bucket key, or sequence numbers don't increase within a bucket.
3. Group the part's minutes by key. A minute is `unbacked` unless every block whose signed time span overlaps its window `[T − 120 min, T)` and whose range starts at or below `H` proves completeness for that window, and the repo bucket of `H`'s block is opened. For each key, build the engine input with `pkg/replay` from those rows and run `pkg/struggle` at the minute's recorded rules version. Compare label, project, credit and cuts with the frozen rows.
4. Mark each frozen row `verified` or `mismatch`; rows of devices with no part stay `unbacked`.

Then build the packet and set `ready`. A part that fails step 1 or 2 sets the submission to `failed` with the reason. The job is idempotent: re-running it gives the same packet.

## Storage

- `submissions`: id, user, project, range, program, status, token hash and encrypted token, trust level at submission, packet (`jsonb`, no link in it), counts, created and decided times.
- `submission_minutes`: the frozen device rows, with `result` (`verified`, `mismatch`, `unbacked`) and the server's result on mismatch.
- `submission_capped`: the frozen capped seconds per minute for the project.
- `submission_parts`: device, gzip bundle (`bytea`), uploaded time, raw rows deleted time.
- `submission_decisions`: every decision, newest wins.
