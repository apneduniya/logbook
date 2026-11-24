# Admin API (Fraud Squad)

Read-only investigation endpoints (ADR 0022). Package: `internal/server/admin`.

## Auth

- `Authorization: Bearer lba_…`, an admin key. Created in `/settings` by a user with `role = 'admin'` (`POST /api/v1/me/admin_keys`, session + CSRF; shown once; `GET` and `DELETE` to list and revoke). A user who stops being an admin loses every key at once.
- The role is set only by `logbook-server admin grant <username>` / `revoke` on the server.
- Session cookies and user API keys get `401` here. A non-admin key doesn't exist.
- Every request is audited as `admin.read` with the admin, path and subject user.
- Rate limit per key: a burst of 300, refilled at 300 per minute.
- Errors: `{"error": "message"}`; `404 {"error": "user not found"}`.

Where Hackatime has the call (`app/controllers/api/admin/v1/*` at `43bde10`), the path, parameters, paging (`limit`, `offset`, `has_more`) and field names match it.

## Users

| Call | Returns |
|---|---|
| `GET /api/admin/v1/user/info?id=` (id, username or email) | id, username, display name, email, created, role, profile public, Hackatime link (id, username, trust level, checked at), device count, last heartbeat, last credited minute |
| `POST /api/admin/v1/user/info_batch` `{"ids": [...]}` (≤ 2,000) | The same, for many users |
| `GET /api/admin/v1/user/heartbeats?id=&start_date=&end_date=&limit=&offset=` | Raw heartbeats, oldest first, `limit` ≤ 5,000, with `has_more` |
| `GET /api/admin/v1/user/projects?id=` | Projects with raw and credited totals, repo URL, hidden, archived |
| `GET /api/admin/v1/user/devices?id=` | Devices (name, OS, created, last seen, revoked) and each one's chain: days sealed, first and last day, gap days, refused conflicting uploads |
| `GET /api/admin/v1/user/minutes?id=&start_date=&end_date=&project=&limit=&offset=` | Credited minutes: device, minute, project, label, credited, cuts, flags, rules version, key |
| `GET /api/admin/v1/user/submissions?id=` | All submissions, any status |
| `GET /api/admin/v1/user/audit_log?id=&limit=&offset=` | Audit entries where the user is actor or subject |

## Submissions

| Call | Returns |
|---|---|
| `GET /api/admin/v1/submissions/{id}` | The submission, its packet, every decision, every part (device, uploaded, bytes, raw rows deleted at) and every frozen minute with its result |
| `GET /api/admin/v1/submissions/{id}/evidence?part=&limit=&offset=` | The part's revealed rows, decoded: block day, leaf index, `seq`, type, time, value. `410` once raw rows are deleted |
