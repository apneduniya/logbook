# Admin API (Fraud Squad)

Read-only investigation endpoints (ADR 0022). Package: `internal/server/admin` (keys, the command line, the routes and their reads); `internal/server/submissions` builds the bundle and decodes parts for it.

## Auth

- `Authorization: Bearer lba_…`, an admin key. Nothing else is read: no cookie, no `api_key` query parameter, no Basic credentials, no other scheme.
- Session cookies, user API keys, OAuth access, refresh and client-credentials tokens, revoked keys and keys of a user whose role is no longer `admin` all get `401 {"error": "HTTP Token: Access denied."}` (Hackatime's text) with `WWW-Authenticate: Bearer realm="Logbook admin"`. Failed authentications are limited per client: a burst of 30, refilled at one every two seconds, then `429`. A valid key is never held back by them.
- A key is `lba_` plus 256 random bits in unpadded base64url (47 characters). Only its SHA-256 is stored (`admin_keys`, migration `00010`), with the first 16 characters (`lba_` and 12 more) in clear as its **prefix**: the key is looked up by its prefix and its hash compared in constant time. The key itself is printed once and never stored, logged or audited.
- Rate limit per key: a burst of 300, refilled at 300 per minute (`429` with `Retry-After`).
- No CORS (`/api/admin/` is on the router's skip list) and `Cache-Control: no-store` on every answer.

### The command line

Admin keys and the role are managed only on the server, with the same environment as the server (it migrates the database first):

| Command | What |
|---|---|
| `logbook-server admin grant <username>` | Sets `role = 'admin'`. Audited `admin.grant` |
| `logbook-server admin revoke <username>` | Sets `role = 'user'` and revokes every admin key of the user (granting again doesn't bring them back). Audited `admin.revoke` with the number of keys revoked |
| `logbook-server admin key create <username> <name>` | Prints a new key once, with its prefix. Refused unless the user is an admin. Names are 1–64 characters without control characters. Audited `admin_key.create` (name and prefix) |
| `logbook-server admin key list` | Every key, oldest first: prefix, user, name, created, last used (written at most once a minute), and `live`, `revoked <time>` or `inactive` (the user isn't an admin). Audited `admin_key.list` |
| `logbook-server admin key revoke <prefix>` | Revokes one live key. Audited `admin_key.revoke` |

Command-line audit rows have no actor and `"via": "command line"` in their data. `logbook-server admin` with no or unknown arguments prints the usage and exits non-zero. `logbook-server` with no arguments still runs the server.

### Audit

Every request with a live key writes `admin.read` audit rows before anything is answered: one per user it read (`subject_type` `user`; every user listed by `info_batch` and `search_fuzzy`), or one about the key (`subject_type` `admin_key`) when it read no user (a `404`, a search with no result, an unsupported path). Each row's actor is the admin, and its data holds the method, the path, the key's prefix, a `request` id shared by all the request's rows, the status of a request that failed, and the request's parameters (query and body), so what was searched for is recorded. Parameters are capped: the ones the API reads (`id`, `user_id`, `ids`, `email`, `query`, `part`, the dates, the filters, `include_ai`, `limit`, `offset`) come first, then the others by name; each value is cut to 1 KiB and they stop at 4 KiB of JSON, and `"params_truncated": true` marks a row where anything was cut or left out. A request that read more than 10 users (only `info_batch` can) records its parameters once, in an extra row about the key that also holds `users` (how many it read); its rows about users hold everything else and point to it by `request` id. So a batch's audit rows grow with the users it read, not with the square of them or with padding. Requests refused by the rate limit read nothing and aren't audited. A request whose audit row can't be written gets `500` and no data. Reads of a submission's evidence are audited before the part is decoded.

### Read-only

No admin endpoint changes user data. The routes read only through `admin.Reads`, whose queries (`store/queries/admin.sql`) are all `SELECT`s, and through the submissions package's `CheckBundle`, `WriteBundle` and `PartRows`, which read. The only writes a request makes are its audit rows and its key's `last_used_at`. Tests check the query file, and that calling every endpoint (writes and unknown paths included) leaves every other table unchanged.

## Hackatime-compatible calls

These match Hackatime at `43bde10` (`config/routes.rb` lines 287–302, `app/controllers/concerns/api/admin/v1/user_utilities.rb`, `app/controllers/concerns/date_parsing.rb`, `app/models/concerns/user_fuzzy_search.rb`): the same HTTP method, parameters, defaults, limits, response envelope, field names and field order, and the same error statuses and messages. Contract tests pin each one (`internal/server/admin/testdata/hackatime/*.golden`; rewrite with `go test ./internal/server/admin -run Contract -update`), and a second test checks every object's keys against Hackatime's lists. Where a Hackatime field has no Logbook source it is `null` (or `[]` for lists, `false` for `hidden`); our extra fields sit under a `logbook` key in each user or row object.

Common rules:
- **The user:** `id`, else `user_id` (Hackatime's `params[:id] || params[:user_id]`), is our numeric public id (ADR 0021). Blank: `422 {"error": "who?"}`. Read as ActiveRecord casts an id (the leading integer, so `12abc` is `12`); no such user: `404 {"error": "user not found"}`.
- **Parameters** come from the query and, on the two `POST`s, a JSON or form body (the body wins). A body over 64 KiB is `413`; malformed JSON is `400`.
- **Dates** (`start_date`, `end_date`), as `DateParsing#parse_ts`: digits are Unix seconds (milliseconds from 10¹² on), 0 to 9999-12-31; otherwise the value must start with a date (`YYYY-MM-DD`, `/` or `.` also accepted; the rest is ignored, as `Date.parse` does): the start of that UTC day, or its last second (23:59:59) for `end_date`. Anything else is `422 {"error": "invalid start_date"}` (or `end_date`). Both bounds are inclusive.
- **Trust levels are unmasked:** admins see `yellow`. A user without a Hackatime link, or whose level couldn't be read (`unknown`), reads as `blue`; `logbook.trust_verified` is then `false` and `logbook.hackatime_trust_level` holds what is stored (`null` without a link).
- Heartbeat text fields Logbook stores as `""` are `null`, as Hackatime stores what a client leaves out. Durations are Hackatime's `duration_seconds`: each heartbeat (by time, then id) is credited its gap to the previous one, at most 120 s, the first 0, summed and rounded to an integer.

| Call | Notes |
|---|---|
| `GET /api/admin/v1/user/info?id=` | `{"user": {...}}`. `admin_level` is `admin` for an admin, else `default`; `suspected` and `banned` follow the unmasked level. `slack_uid`, `slack_username`, `country_code` are `null`; `github_username` is the one Hackatime reported at linking. `email_addresses` is the account's email. `api_keys_count` counts live user API keys. `last_heartbeat_at` and `stats` cover heartbeats from 2000 to 2100 (Hackatime's validity window); `days_active` counts UTC days. `logbook`: `username`, `hackatime_id`, `trust_verified`, `hackatime_trust_level`, `trust_checked_at`, `role`, `profile_public`, `devices` (`active`, `revoked`), `last_credited_minute` |
| `GET /api/admin/v1/user/info_batch?ids=1,2,3` | Hackatime's parsing: split on `,`, trimmed, each read as `to_i` (so `abc` is 0, which names nobody), duplicates dropped, the first 2,000 kept. Blank: `422 {"error": "ids parameter required"}`; nothing left: `422 {"error": "no valid ids provided"}`. `{"users": [...]}` by public id, each with `as_json`'s columns then methods: `id, username, slack_uid, slack_username, github_username, timezone, country_code, trust_level, slack_avatar_url, github_avatar_url, display_name, avatar_url`; `logbook`: `username`, `hackatime_id`, `trust_verified`. Unknown ids are left out |
| `GET /api/admin/v1/user/heartbeats?id=&limit=&offset=&…` | Filters `start_date`, `end_date`, and exact `project`, `language`, `entity`, `editor`, `machine` when present. `limit` 1–5,000 (default 1,000; `to_i`, then clamped), `offset` ≥ 0. By time then id. `include_ai` (Rails boolean: anything but empty, `0`, `f`, `false`, `off`) adds `ai_model, ai_session, ai_subscription_plan, ai_input_tokens, ai_output_tokens, ai_prompt_length, ai_line_changes, human_line_changes` after `source_type`. `{"user_id", "heartbeats", "total_count", "has_more"}` with `has_more = offset + limit < total_count`. Each heartbeat: `time` in fractional Unix seconds, `source_type` `direct_entry` or `wakapi_import` (imported from Hackatime), `ip_address`, `ai_subscription_plan` and `ja4` `null` (not recorded), `hidden` `false` (Logbook has no exclusions). `logbook`: `source` (`editor`, `agent`, `import`), `plugin`, `fields_hash`. File paths are included: admins see raw heartbeats (ADR 0022) |
| `GET /api/admin/v1/user/projects?id=&start_date=&end_date=` | Every named project of the user's heartbeats, most heartbeats first (then by name); with either date, `start_date` defaults to ten years ago and `end_date` to the end of today. `{"user_id", "username" (the display name), "projects": [{name, total_heartbeats, total_duration, first_heartbeat, last_heartbeat, languages, repo, repo_mapping_id (null), archived}], "total_projects"}`. `logbook`: `credited_seconds` (counted, capped time over the same span) and `hidden`. Hidden projects are listed: admins see everything |
| `GET /api/admin/v1/user/trust_logs?id=` | The newest 25 trust levels Logbook read for the user from Hackatime (`hackatime_trust_history`, one row per stretch of an unchanged level), newest first, in Hackatime's shape: `{"trust_logs": [{id, previous_trust_level, new_trust_level, changed_by, reason, notes, created_at, edited_at, edited_by, original_reason, original_notes}]}`. `previous_trust_level` is the level read before (`null` for the first ever read); `created_at` is when the level was first read; `changed_by`, `reason`, `notes` and the edit fields are `null` (Hackatime decides levels, not us). A level may be `unknown` (Logbook couldn't read it). `logbook`: `first_checked_at`, `last_checked_at`. Rows from before migration `00010` only know their latest reading, so their first equals their last |
| `POST /api/admin/v1/user/get_user_by_email` | `email`, matched exactly. Blank: `422 {"error": "bro dont have a email"}`; none: `404 {"error": "email not found"}`; else `{"user_id": <public id>}` (the oldest account when several share the email) |
| `POST /api/admin/v1/user/search_fuzzy` | `query` (trimmed). Blank: `422 {"error": "bro dont have a query"}`. `{"users": [{id, username, slack_username, github_username, slack_avatar_url, github_avatar_url, email, rank_score}]}`, at most 10, ranked as `fuzzy_ranked_search` ranks them: candidates contain the term (case-insensitive, `%`, `_` and `\` matched literally) in the username, display name, GitHub username or email, or have it as their public id; each of those four fields scores 100 for an exact match, 50 for a prefix and 10 for a substring, the public id 1000; best first, then by username. Users without an email are left out (Hackatime's gate). There is no Slack: Slack fields are `null` and Slack ids aren't searched. The GitHub username is the one Hackatime reported at linking |

Not provided (they have no meaning for us, or need data we don't keep): `check`, `user/heartbeat_values`, `get_users_by_ip`, `get_users_by_machine`, `user/stats` (use the program API), `banned_users`, `user/convict`, `poison`, `ban` and every other write, `admin_api_keys`, `leaderboard_shadowbans`, `trust_level_audit_logs`, `deletion_requests`, `timeline`, `heartbeats/by_user_agent_segment`. Any other path under `/api/admin/`, and a provided path with another method (`POST user/info`), is `404 {"error": "not found"}` after authentication. A tool that needs one gets `404` and we document the patch.

## Logbook calls

The user is named as above (`id` or `user_id`, `422`/`404` alike). Pages are `limit` and `offset` (`to_i`, clamped) with `{"limit", "offset", "has_more"}` beside the list. Times are RFC 3339 UTC.

| Call | Returns |
|---|---|
| `GET /api/admin/v1/user/devices?id=` | `{"user_id", "devices": [...]}`, oldest first, revoked ones included: `id, name, os, public_key` (standard base64), `created_at, last_seen_at, revoked_at`, and `chain`: `days_sealed` (stored evidence days), `days_with_block` (those with ADR 0019 block fields, the only ones a submission can be checked against), `first_day`, `last_day` (`null` without any), `gap_days` and `gaps` (`[{from, to}]`, inclusive, the days between first and last with no record), `refused_uploads` (`[{day, at, detail}]`: every different, validly signed record the server refused for a stored day, the ledger's `evidence_conflict` audit entries, with both versions' hashes and seq ranges in `detail`) |
| `GET /api/admin/v1/user/minutes?id=&start_date=&end_date=&project=&limit=&offset=` | `{"user_id", "minutes": [...], "limit", "offset", "has_more"}`: every credited row (every device, label and project) by minute, device, project: `device_id, minute, project, label, credited, cuts, flags, rules_version, key ({computed_at, snapshot}, null before ADR 0017), updated_at`. Dates as in the Hackatime calls; `limit` 1–5,000 (default 1,000) |
| `GET /api/admin/v1/user/submissions?id=` | `{"user_id", "submissions": [...]}`, every submission of any status, newest first (the object below) |
| `GET /api/admin/v1/user/audit_log?id=&limit=&offset=` | `{"user_id", "entries": [{id, action, actor ({id, username}, null for none or a deleted account), subject_type, subject_id, data, created_at}], "limit", "offset", "has_more"}`, newest first: entries where the user is the actor, or the subject is the user, one of their devices or one of their submissions (other admins' `admin.read` rows about them included). `limit` 1–1,000 (default 100) |

## Submissions

A submission `{id}` that isn't a UUID or doesn't exist is `404 {"error": "submission not found"}`; the audit subject is the submission's user. The submission object:

```json
{"id": "…", "user": {"id": 100000002, "username": "orpheus"}, "project": "terra-bot", "from": "2026-11-01", "to": "2026-11-20",
 "program": {"slug": "terra", "name": "Terra"}, "status": "ready", "error": "",
 "trust_at_submission": "yellow", "hackatime_user_id": 4242,
 "minutes": {"verified": 744, "mismatch": 0, "unbacked": 0}, "counted_seconds": 44640, "raw_seconds": 61200,
 "parts_version": 2, "parts": 2, "raw_deleted": false,
 "created_at": "…", "verified_at": "…", "decided_at": "…", "withdrawn_at": null,
 "decision": {"id": 7, "value": "adjusted", "approved_seconds": 40000, "reason": "…", "by": {"id": 100000009, "username": "rita"}, "at": "…"}}
```
`trust_at_submission` is unmasked (`null` when Hackatime wasn't linked); `decision` is the latest, `null` without one; `raw_deleted` is true once the parts' raw rows are gone.

| Call | Returns |
|---|---|
| `GET /api/admin/v1/submissions/{id}` | `{"submission", "packet" (null unless ready), "decisions" (every one, newest first), "parts": [{device_id, device_name, bytes, uploaded_at, raw_deleted_at}], "minutes": [...]}`: every frozen row, by device and minute, with `device_id, minute, label, credited, cuts, flags, rules_version, key, result` (`verified`, `mismatch`, `unbacked`, null while verifying), `reason` (why it is unbacked) and `server` (`{label, credited, cuts}`, the replay's row on a mismatch, else null) |
| `GET /api/admin/v1/submissions/{id}/evidence?part=&limit=&offset=` | The opened rows of one part, decoded, in block, bucket and sequence order: `{"submission_id", "device_id", "rows": [{day, bucket, bucket_minute, seq, type, time, value}], "total", "limit", "offset", "has_more"}`. `part` is the device id; it may be left out when the submission has exactly one part (`400` otherwise; `404` for a device without a part). `bucket` is the bucket key (0 for the repo bucket, whose `bucket_minute` is null); `value` is the row as hashed (the line's `v`). `limit` 1–5,000 (default 1,000). The part is decoded within one of the server-wide submission decode slots (`submissions.DecodeSlots`, shared with uploads and the re-check), waiting up to a minute for one (`503` after that), and dropped after the page is cut. `410 {"error": "the submission's raw evidence was deleted"}` once raw rows are deleted |
| `GET /api/admin/v1/submissions/{id}/bundle` | A gzip file (`Content-Type: application/gzip`, `Content-Disposition: attachment; filename="submission-<id>.json.gz"`) for `logbook verify` (ADR 0020; format in [submissions.md](submissions.md#the-verification-bundle)): every part as uploaded, the devices' signed evidence records and public keys, the frozen minutes, capped seconds and cap inputs (`submission_cap_inputs`), all frozen in the one snapshot taken at submission. `410` once raw rows are deleted (checked before anything is sent) |

**Memory.** The bundle is streamed (`submissions.Checker.WriteBundle`): the frozen rows, capped seconds, cap inputs and evidence records are read first, then each stored part's JSON is copied out of its gzip into the response one part at a time, never decoded. So an export holds at most one stored part's compressed bytes (no more than an upload can store) plus those small rows, needs no decode slot, and can't add to the decode memory bound. If reading fails after the response has started, the connection is cut, so the client sees a truncated gzip stream rather than a bundle that looks whole.
