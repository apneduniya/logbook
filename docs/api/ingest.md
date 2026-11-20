# Ingest API

WakaTime-compatible heartbeat ingest (ADR 0005) and the status-bar total. Package `internal/server/ingest`.

Every route exists under two prefixes:

- `/api/v1`: WakaTime's layout. Set `api_url = https://<host>/api/v1` in `~/.wakatime.cfg`.
- `/api/hackatime/v1`: Hackatime's layout, so a user can change only the host in an existing Hackatime `api_url`.

The two behave the same except for the success status of the single-heartbeat endpoint.

## Authentication

All routes require a Logbook API key (`RequireAPIKey`), sent the way wakatime-cli sends it: `Authorization: Basic base64(<key>)`, or `Authorization: Bearer <key>`. A missing or invalid key returns `401 {"error": "unauthorized"}`.

## Rate limit

Heartbeat routes allow each user a burst of 120 requests, refilled at 2 per second (per server process). Over the limit: `429 {"error": "Too many requests, try again later"}` with `Retry-After: 60`. wakatime-cli puts the heartbeats back in its offline queue and retries later.

## Request headers

wakatime-cli sends these, and the server uses them:

| Header | Use |
|---|---|
| `User-Agent` | Used as the heartbeat's `user_agent` when the body has neither `user_agent` nor `plugin`. |
| `X-Machine-Name` | Query-escaped hostname. Overrides the body's `machine`. |
| `X-Logbook-Source: agent` | Set by the Logbook agent; marks heartbeats as `source = agent` instead of `editor`. |

The `user_agent` is parsed (format `wakatime/<v> (<os>-<kernel>-<arch>) <runtime> <editor>/<v> <editor>-wakatime/<v>`). The editor and operating system it names override the body's `editor` and `operating_system`, and the plugin token fills an empty `plugin`. This matches Hackatime.

## Heartbeat object

WakaTime's format plus Hackatime's AI fields (`pkg/wire.Heartbeat`). Unknown fields are ignored.

| Field | Type | Notes |
|---|---|---|
| `entity` | string | Required, at most 1024 bytes |
| `time` | number | Required. Unix seconds with fraction; not before 2013 and at most 1 hour in the future |
| `type` | string | Default `file` |
| `category` | string | Default `coding` |
| `project`, `branch`, `language`, `editor`, `plugin`, `machine`, `operating_system`, `user_agent` | string | At most 256 bytes each |
| `is_write` | bool | |
| `lines`, `lineno`, `cursorpos`, `line_additions`, `line_deletions`, `project_root_count` | int | 0 to 2147483647 |
| `dependencies` | string[] | At most 100, each at most 256 bytes |
| `ai_model`, `ai_session` | string | At most 256 bytes |
| `ai_input_tokens`, `ai_output_tokens` | int | Not negative |
| `ai_prompt_length`, `ai_line_changes`, `human_line_changes` | int | 0 to 2147483647 |

The server removes NUL bytes from every string, strips control characters from `project`, and trims it. Heartbeats are deduplicated per user on all their fields: resending one is accepted and reported with the same `id`, but stored once.

Request bodies are limited to 1 MiB.

## POST `/api/v1/users/current/heartbeats`

Also `POST /api/hackatime/v1/users/current/heartbeats`. Stores one heartbeat.

**Request:** one heartbeat object, or an array containing exactly one.

**Response:** `201 Created` under `/api/v1` (WakaTime) or `202 Accepted` under `/api/hackatime/v1` (Hackatime):

```json
{
  "data": {
    "id": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
    "entity": "/home/me/app/main.go",
    "type": "file",
    "category": "coding",
    "time": 1763546400.123456,
    "project": "app",
    "branch": "main",
    "language": "Go",
    "editor": "vscode",
    "machine": "my-laptop",
    "operating_system": "linux",
    "user_agent": "wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 vscode/1.95.3 vscode-wakatime/24.9.1",
    "is_write": true
  }
}
```

`id` is the hex SHA-256 fields hash of the stored heartbeat.

**Errors:**

| Status | When |
|---|---|
| 400 | Body isn't JSON, isn't an object or array, is an empty array, has more than one heartbeat, or the heartbeat is invalid (message says which field) |
| 401 | No valid API key |
| 413 | Body over 1 MiB |
| 429 | Rate limited |

## POST `/api/v1/users/current/heartbeats.bulk`

Also `POST /api/hackatime/v1/users/current/heartbeats.bulk`. This is the endpoint wakatime-cli uses.

**Request:** an array of 1 to 100 heartbeat objects (a single object is accepted as an array of one).

**Response:** `201 Created`, with one `[body, status]` pair per heartbeat, in request order:

```json
{
  "responses": [
    [{"data": {"id": "3f39...", "entity": "/tmp/main.go", "...": "..."}}, 201],
    [{"error": "invalid heartbeat: entity is required"}, 400]
  ]
}
```

- `201`: stored (or already stored). `data` is the same object the single endpoint returns.
- `400`: this heartbeat is invalid. wakatime-cli drops it instead of retrying. The other heartbeats are still stored.

**Errors (whole request):**

| Status | When |
|---|---|
| 400 | Body isn't JSON, isn't an array or object, is empty, or has more than 100 heartbeats (`"Too many heartbeats in a single request (max 100)"`) |
| 401 | No valid API key |
| 413 | Body over 1 MiB |
| 429 | Rate limited |
| 500 | Database failure; nothing from the batch was stored. wakatime-cli re-queues the batch |

## GET `/api/v1/users/current/statusbar/today`

Also `GET /api/hackatime/v1/users/current/statusbar/today`. Today's coding time in the user's timezone (`users.timezone`; UTC if unknown), from the per-minute rollups. Used by `wakatime-cli --today` and editor status bars.

**Response:** `200 OK`, WakaTime's shape:

```json
{
  "cached_at": "2026-11-19T12:00:00Z",
  "data": {
    "categories": [
      {"decimal": "1.02", "digital": "1:01:00", "hours": 1, "minutes": 1, "name": "Coding",
       "percent": 100, "seconds": 0, "text": "1 hr 1 min", "total_seconds": 3660}
    ],
    "dependencies": [],
    "editors": [{"name": "vscode", "...": "..."}],
    "grand_total": {"decimal": "1.02", "digital": "1:01", "hours": 1, "minutes": 1,
                    "text": "1 hr 1 min", "total_seconds": 3660},
    "languages": [{"name": "Go", "...": "..."}],
    "machines": [],
    "operating_systems": [],
    "projects": [
      {"decimal": "1.00", "digital": "1:00:00", "hours": 1, "minutes": 0, "name": "logbook",
       "percent": 98.36, "seconds": 0, "text": "1 hr", "total_seconds": 3600},
      {"name": "Unknown", "percent": 1.64, "total_seconds": 60, "...": "..."}
    ],
    "range": {"date": "2026-11-19", "start": "2026-11-18T18:30:00Z", "end": "2026-11-19T18:29:59Z",
              "text": "Thu Nov 19th 2026", "timezone": "Asia/Kolkata"}
  },
  "has_team_features": false
}
```

- Every list is an array, never `null`. Lists are sorted by time, longest first.
- `machines`, `operating_systems` and `dependencies` are always empty: rollups don't keep them.
- Heartbeats without a project are counted under `Unknown`; an empty language is `Other`, an empty editor `Unknown`.
- `text` follows WakaTime: `2 hrs 17 mins`, `1 hr`, `49 mins`, `7 secs`; `0 secs` when there's no activity.

**Errors:** `401` without a valid API key, or if the key's user no longer exists.

## How time is counted

See `internal/server/ingest/rollup/doc.go`. In short: each heartbeat is credited the gap to the user's next heartbeat (across all machines), up to 2 minutes; longer gaps credit nothing. Time is stored per minute, never more than 60 seconds per minute.
