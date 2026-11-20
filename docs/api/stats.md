# Stats, projects, profiles and live API

The JSON API the web UI uses for the dashboard, project pages, profiles and
live updates. Packages: `internal/server/stats`, `internal/server/projects`,
`internal/server/profiles` and `internal/server/live`.

## Conventions

- **Auth:**
  - **session** means a signed-in browser session cookie.
  - Requests that change state (PATCH) also need the `X-CSRF-Token` header.
  - Without a session the response is `401 {"error": "not signed in"}`.
  - **public** means no auth is needed.
- **Errors** are `{"error": "message"}`. Any 5xx is `{"error": "internal error"}`.
- **Times** are RFC 3339 in UTC. **Dates** are `YYYY-MM-DD` local dates in the user's time zone (`timezone` on their profile).
- **Credited time** is time on minutes labelled `you`. Every `seconds` and `total_seconds` field counts only credited time. Agent and idle time appear only under `labels`.
- Every `/me` endpoint reads and writes only the signed-in user's data.

---

## Stats

### `GET /api/v1/me/summary`

Auth: session.

| Query | Required | Meaning |
|---|---|---|
| `start` | no | First local date (inclusive). Default: `end` minus 6 days |
| `end` | no | Last local date (inclusive). Default: today in the user's time zone |
| `project` | no | Only count this project (exact name, ≤ 256 bytes). Absent means all projects |

The range can be at most 366 days.

`200`:

```json
{
  "start": "2026-11-14",
  "end": "2026-11-20",
  "timezone": "Asia/Kolkata",
  "total_seconds": 12345,
  "labels": { "you": 12345, "agent": 3600, "idle": 600 },
  "projects":   [{ "name": "logbook", "seconds": 10000 }],
  "languages":  [{ "name": "Go", "seconds": 9000 }],
  "editors":    [{ "name": "vscode", "seconds": 12345 }],
  "categories": [{ "name": "coding", "seconds": 12000 }],
  "days": [{ "date": "2026-11-14", "seconds": 0 }, { "date": "2026-11-15", "seconds": 1800 }]
}
```

- Lists are sorted by `seconds` (largest first), then by name. They are `[]` when empty.
- `days` has one entry for every date in the range, with zeros filled in.
- A day is a local calendar day, so a DST change day has 23 or 25 hours.

Errors:
- `400` when `start` or `end` isn't a valid date, `start` is after `end`, the range is longer than 366 days, or `project` is invalid.
- `401` without a session.

### `GET /api/v1/me/today`

Auth: session. Returns today's summary in the user's time zone, plus their streak.

`200`: the same fields as the summary (with `start` and `end` both today), plus:

```json
{ "streak": { "current": 4, "longest": 21 } }
```

- A day counts towards a streak when it has at least 15 minutes of credited time.
- `current` counts back from today. If today hasn't reached 15 minutes yet, it counts back from yesterday, so the streak isn't broken before the day ends.
- `longest` covers all of the user's history.

Errors: `401`.

---

## Projects

A project appears once it has tracked time or saved settings.

`Project` object:

```json
{
  "name": "logbook",
  "total_seconds": 36000,
  "last_activity": "2026-11-20T10:31:00Z",
  "languages": [{ "name": "Go", "seconds": 30000 }],
  "repo_url": "https://github.com/apneduniya/logbook",
  "hidden": false,
  "archived": false
}
```

- `total_seconds` is all-time credited time.
- `last_activity` is the end of the last tracked minute of any label. It is `null` when the project has settings but no tracked time.
- `hidden` projects are left out of the public profile entirely, including its totals and languages.

### `GET /api/v1/me/projects`

Auth: session. `200`: `{"projects": [Project, ...]}`, most recently active first. Projects with no activity come last, sorted by name.

Errors: `401`.

### `GET /api/v1/me/projects/{name}`

Auth: session. URL-encode `{name}`, for example `a%2Fb` for `a/b`. `200`: `Project`.

Errors:
- `400` when the name is empty or longer than 256 bytes.
- `401` without a session.
- `404` when the user has no such project. This includes another user's project.

### `PATCH /api/v1/me/projects/{name}`

Auth: session + CSRF. Body: any subset of the fields below. Absent or `null` fields are left unchanged.

```json
{ "repo_url": "https://github.com/owner/repo", "hidden": true, "archived": false }
```

- `repo_url` must be exactly `https://github.com/<owner>/<repo>`, with no trailing slash, path, query or fragment. `""` clears it.

`200`: the updated `Project`. Each change is written to the audit log (`project.update`, with the old and new values).

Errors:
- `400` for an invalid `repo_url`, an empty patch, or invalid JSON.
- `401` without a session.
- `404` when the user has no such project.
- `413` when the body is larger than 1 MiB.

---

## Profiles

### `GET /api/v1/users/{username}/profile`

Auth: public. `{username}` is case-insensitive.

`200`:

```json
{
  "username": "alice",
  "display_name": "Alice",
  "avatar_url": "https://...",
  "member_since": "2026-11-01T08:00:00Z",
  "totals": { "all_time_seconds": 360000, "last_7_days_seconds": 36000 },
  "projects": [
    { "name": "logbook", "repo_url": "https://github.com/apneduniya/logbook",
      "total_seconds": 300000, "last_7_days_seconds": 30000 }
  ],
  "languages": [{ "name": "Go", "seconds": 250000 }]
}
```

- `projects` lists the top 10 non-hidden projects by all-time credited time.
- `last_7_days` is the last 168 hours, not 7 calendar days.
- Email, HCA id and time zone are never included.

Errors: `404 {"error": "not found"}`. The response is identical whether the user doesn't exist, the username is malformed, or the profile is private.

### `GET /api/v1/me/profile`

Auth: session.

`200`:

```json
{
  "username": "alice",
  "display_name": "Alice",
  "avatar_url": "https://...",
  "timezone": "Asia/Kolkata",
  "profile_public": true,
  "member_since": "2026-11-01T08:00:00Z"
}
```

Errors: `401`.

### `PATCH /api/v1/me/profile`

Auth: session + CSRF. Body: any subset of the fields below. Absent fields are left unchanged.

```json
{ "display_name": "Alice", "timezone": "Asia/Kolkata", "profile_public": false }
```

- `display_name` is trimmed and can be at most 64 characters. Control characters and bidirectional-override characters are rejected. `""` is allowed.
- `timezone` must be an IANA zone name, such as `Asia/Kolkata` or `UTC`. `Local` is rejected.

`200`: the updated profile, in the same shape as `GET /api/v1/me/profile`. Each change is audited (`profile.update`).

Setting `profile_public` to `false` takes effect at once: the public profile returns `404`, and any open public live streams for the user are closed.

Errors:
- `400` for an invalid field, an empty patch, or invalid JSON.
- `401` without a session.
- `413` when the body is too large.

---

## Live updates (Server-Sent Events)

Use `EventSource`. Streams carry notifications only. When one arrives, refetch the data you show (summary, today, profile).

- A `retry` hint (5 s) is sent first, then `event: ready` with `data: {}`.
- A `: keepalive` comment is sent every 25 s.
- The server ends a stream after 30 minutes. `EventSource` reconnects on its own, and authentication is checked again on reconnect.
- If the client is slow, several changes are merged into one event, so updates are never queued up or lost.

### `GET /api/v1/me/live`

Auth: session.

```
event: minutes
data: {"from":"2026-11-20T10:00:00Z","to":"2026-11-20T10:05:00Z"}
```

`from` and `to` cover every minute that changed since the last event (`to` is exclusive).

Errors:
- `401` without a session.
- `429` when the user already has 5 streams open.
- `503` when the server is at its stream limit or shutting down.

### `GET /api/v1/users/{username}/live`

Auth: public. Only for public profiles.

```
event: minutes
data: {}
```

The event carries no time range or project. The stream is closed when the profile is made private.

Errors:
- `404` (same as the profile endpoint).
- `429` when 100 viewers are already watching this profile.
- `503`.
