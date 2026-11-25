# Admin API (Fraud Squad)

Read-only investigation endpoints (ADR 0022). Package: `internal/server/admin`.

## Auth

- `Authorization: Bearer lba_…`, an admin key. Keys are made only on the server (`logbook-server admin key create <username> <name>`, printed once; `admin key list`, `admin key revoke <prefix>`), for a user with `role = 'admin'`. Revoking the role revokes every key at once.
- The role is set only by `logbook-server admin grant <username>` / `revoke`.
- Session cookies and user API keys get `401` here.
- Every request is audited as `admin.read` with the admin, path and subject user.
- Rate limit per key: a burst of 300, refilled at 300 per minute.
- Errors: `{"error": "message"}`; `404 {"error": "user not found"}`.

## Hackatime-compatible calls

These match Hackatime at `43bde10` (`config/routes.rb` lines 287–302, `app/controllers/concerns/api/admin/v1/user_utilities.rb`): the same HTTP method, parameters, defaults, limits, response envelope and field names. Contract tests pin each one. Where a Hackatime field has no Logbook source it is `null` (or `[]` for lists); our extra fields sit under a `logbook` key in each user or row object.

| Call | Notes |
|---|---|
| `GET /api/admin/v1/user/info?id=` | Envelope `{"user": {...}}`. `id` is our numeric public id (ADR 0021). `trust_level` from the Hackatime link (unmasked: admins see `yellow`). `logbook`: username, devices, last credited minute, Hackatime id |
| `GET /api/admin/v1/user/info_batch?ids=1,2,3` | Comma list, at most 2,000; `{"users": [...]}` |
| `GET /api/admin/v1/user/heartbeats?id=&limit=&offset=&…` | Hackatime's filters, `limit` 1–5,000 (default 1,000), `include_ai`, and its paging fields including `total_count` and `has_more` |
| `GET /api/admin/v1/user/projects?id=&start_date=&end_date=` | Hackatime's shape; `logbook` adds credited seconds per project |
| `GET /api/admin/v1/user/trust_logs?id=` | The newest 25 trust levels we have read for the user from Hackatime (a history table kept from step 5 on), newest first, in Hackatime's shape |
| `POST /api/admin/v1/user/get_user_by_email` | `email` |
| `POST /api/admin/v1/user/search_fuzzy` | `query`, at most 10 results, ranked as Hackatime ranks them, over `public_id`, email, username and display name. GitHub username isn't stored, so it isn't searched (exception) |

Not provided (they have no meaning for us, or need data we don't keep): `user/heartbeat_values`, `get_users_by_ip`, `get_users_by_machine`, `user/stats` (use the program API), `user/convict` and other writes, `timeline`, `heartbeats/by_user_agent_segment`. A tool that needs one gets `404` and we document the patch.

## Logbook calls

| Call | Returns |
|---|---|
| `GET /api/admin/v1/user/devices?id=` | Devices (name, OS, created, last seen, revoked) and each one's chain: days sealed, first and last day, gap days, refused conflicting uploads |
| `GET /api/admin/v1/user/minutes?id=&start_date=&end_date=&project=&limit=&offset=` | Credited minutes: device, minute, project, label, credited, cuts, flags, rules version, key; `has_more` |
| `GET /api/admin/v1/user/submissions?id=` | All submissions, any status |
| `GET /api/admin/v1/user/audit_log?id=&limit=&offset=` | Audit entries where the user is actor or subject |

## Submissions

| Call | Returns |
|---|---|
| `GET /api/admin/v1/submissions/{id}` | The submission, its packet, every decision, every part (device, uploaded, bytes, raw rows deleted at) and every frozen minute with its result |
| `GET /api/admin/v1/submissions/{id}/evidence?part=&limit=&offset=` | The part's opened rows, decoded: block day, bucket key, `seq`, type, time, value. `410` once raw rows are deleted |
| `GET /api/admin/v1/submissions/{id}/bundle` | A gzip JSON file for `logbook verify` (ADR 0020; format in [submissions.md](submissions.md#the-verification-bundle)): every part as uploaded, the devices' signed evidence records and public keys, the frozen minutes, capped seconds and cap inputs (`submission_cap_inputs`), all frozen in the one snapshot taken at submission. `410` once raw rows are deleted |
