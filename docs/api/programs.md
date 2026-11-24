# Programs, OAuth and the Hackatime-shaped program API

How a YSWS program connects to Logbook (ADR 0008, ADR 0021). Packages: `internal/server/oauth` (apps, grants, tokens), `internal/server/programs` (members), `internal/server/programapi` (the Hackatime-shaped read endpoints, using `internal/server/stats` for streaks).

**Compatibility rule:** where a call exists on Hackatime, its path, parameters, status codes and JSON shape match Hackatime at commit `43bde10` exactly (`app/controllers/api/v1/stats_controller.rb`, `app/controllers/api/v1/authenticated/*`, `app/controllers/api/v1/my/heartbeats_controller.rb`, `app/services/project_stats_query.rb`, `lib/wakatime_service.rb`, `config/initializers/doorkeeper.rb`). Fields we add are extra keys that existing clients ignore. Contract tests in `internal/server/programapi/testdata/hackatime/` pin each request a program repo makes (Stardance, Flavortown, Save Point, Crossroads, Ari, the 11 project-picker repos) with its full response; run `go test ./internal/server/programapi -run Contract -update` to rewrite them after a deliberate change.

## Apps (programs)

Session auth; writes need CSRF.

| Call | What |
|---|---|
| `GET /api/v1/me/apps` | Apps the user owns or reviews for |
| `POST /api/v1/me/apps` | `{name, slug, redirect_uris[], homepage_url, public}`. Returns the app with `client_id` and `client_secret` (shown once). Max 10 apps per user. Audited `app.create` |
| `PATCH /api/v1/me/apps/{id}` | Owner only. Name, redirect URIs, homepage, public |
| `POST /api/v1/me/apps/{id}/secret` | Owner only. Rotates the secret, returns it once. Audited |
| `DELETE /api/v1/me/apps/{id}` | Owner only. Revokes all its tokens. Submissions keep the program's name but lose decisions-in-progress rights. Audited |
| `GET /api/v1/me/apps/{id}/members`, `POST` `{username}`, `DELETE /{username}` | Owner only for writes. Members are reviewers (ADR 0020) |
| `GET /api/v1/me/apps/{id}/submissions` | Members. Submissions addressed to the program, newest first, filter `status`, `decision` |
| `GET /api/v1/programs` | Public programs (`slug`, `name`, `homepage_url`), for the submit picker |

- `slug`: 2–40 of `a-z0-9-`, unique.
- `redirect_uris`: 1–5, absolute, no fragment; `https://`, or `http://localhost` / `127.0.0.1` with any port. Matching is exact.

## OAuth 2

### `GET /oauth/authorize`

Query: `client_id`, `redirect_uri`, `response_type=code`, `scope` (space-separated; default `profile`), `state`, optional `code_challenge` + `code_challenge_method=S256` (required when the app has no secret).

- Invalid client or redirect URI: an error page on the web UI. Never a redirect.
- Other errors: redirect to `redirect_uri` with `error` and `state` (RFC 6749 §4.1.2.1).
- Otherwise `302` to `{LOGBOOK_WEB_URL}/oauth/authorize?request=<id>`: a 10-minute pending request, stored server side. If the user is signed out, the web UI sends them to sign in first.
- If the user already granted at least these scopes to this app, the code is issued straight away (`302` to `redirect_uri?code=…&state=…`).

### `GET /api/v1/oauth/requests/{id}`

Session. The pending request for the consent page: app name, owner username, homepage, scopes with plain descriptions.

### `POST /api/v1/oauth/requests/{id}`

Session + CSRF. `{"approve": true|false}`. Returns `{"redirect": "<redirect_uri>?code=…&state=…"}` (or `error=access_denied`). Single use. Audited `oauth.grant`.

### `POST /oauth/token`

Form-encoded. Client authentication by HTTP Basic or `client_id`/`client_secret` in the body (Doorkeeper accepts both).

- `grant_type=authorization_code`: `code`, `redirect_uri`, `code_verifier` when a challenge was sent.
- `grant_type=refresh_token`: `refresh_token`. Rotates the refresh token.
- `grant_type=client_credentials`: scope `submissions`.

`200` (Doorkeeper's shape):
```json
{"access_token": "…", "token_type": "Bearer", "expires_in": 31536000, "refresh_token": "…", "scope": "profile read", "created_at": 1795000000}
```
Errors per RFC 6749 §5.2 (`invalid_grant`, `invalid_client` with `401`, …).

### `POST /oauth/revoke`

RFC 7009. Always `200`.

### User grants

`GET /api/v1/me/grants`, `DELETE /api/v1/me/grants/{app_id}` (session, CSRF): what apps can read your data, and revoking them.

## The program API

Bearer token from OAuth, or none where marked public. Package `internal/server/programapi`; the server injects an `Authenticator` that resolves OAuth access tokens (package `oauth`) and user API keys (package `auth`).

**Credentials, per call, as on Hackatime:**
- `authenticated/*` (Doorkeeper): only `Authorization: Bearer <access token>`. No token, an unknown, expired or revoked one, or an API key: `401` with an empty body and `WWW-Authenticate: Bearer realm="Doorkeeper", error="invalid_token", …`. A token without the call's scope: `403`, empty body, `error="insufficient_scope"`. A token in the query string isn't read.
- `users/{id}/*` (stats controller): `Authorization: Bearer` with a user API key or an OAuth token that has `read` (Hackatime's `api_user_from_credentials(oauth_scopes: ["read"])`). A credential that doesn't verify, or lacks `read`, is ignored (the call goes on as anonymous); it never causes a `401`. Basic auth isn't read here.
- `my/heartbeats`: a user API key as `Bearer` or `Basic base64(key)`, or an OAuth token with `read`. Otherwise `401 {"error": "Unauthorized"}`.
- User API keys are accepted on exactly these two families because Hackatime accepts its own API key there (Stardance reads `users/my/stats` and `my/heartbeats` with the user's key). They are never accepted on `authenticated/*`.
- **Red users** (Hackatime trust `red`): Hackatime treats their credentials as absent (`api_access_restricted?`), so `authenticated/projects` and `my/heartbeats` answer `401 {"error": "Unauthorized"}` and `users/{id}/*` treats them as anonymous. `authenticated/me` still answers, as on Hackatime.

**User ids.** Hackatime ids are integers, and typed clients (including our own Hackatime client) decode `id` as one. Every user gets a numeric `public_id` (a `bigint` identity column starting at `100000001`, never reused, so it can't collide with a Hackatime id a program stored earlier), and every Hackatime-shaped response puts it in `id` (`data.user_id` as a string in stats, as Hackatime does). A path identifier is resolved as Hackatime does at `43bde10` (`StatsController#set_user`, `User.lookup_by_identifier`):
1. on the stats-controller endpoints (`stats`, `heartbeats/spans`, `projects`, `project/{name}`, `projects/details`), `my` is the user of the credential (a `read` token or an API key); without one, `404 {"error": "User not found"}`. Not on `trust_factor`, where `my` names nobody (Hackatime's `trust_factor` doesn't run `set_user`);
2. digits only: `public_id`, and if no user has it, a username equal to those digits;
3. otherwise a username (case-insensitive).

An identifier that names nobody, or can't be a username: `404 {"error": "User not found"}`.

Compatibility exceptions, documented and covered by tests: Slack ids and HCA ids aren't accepted in V1 (HCA doesn't give us the Slack id), so Crossroads, which looks users up by Slack id, needs a small patch. `my` is reserved and can't be a username (sign-up skips it; the database refuses it).

**Hidden projects** are invisible to programs on every call, even with the user's own token: left out of lists, totals, `filter_by_project` results, spans and heartbeats.

**Time.** "Raw" is time at the keyboard from the heartbeat rollup (`user_minutes`, label `you`, the Hackatime-equivalent figure); "counted" is capped credited time from `credited_project_minutes` (ADR 0014). A user who never ran the agent has `credited_seconds: 0`. Range bounds: minutes starting in `[start, end)`; heartbeats (spans, counts, `my/heartbeats` rows) with `start <= time <= end`.

**The `logbook` extension.** Any Hackatime-shaped response may carry one extra top-level object, `logbook`, which Hackatime clients ignore. Its fields, all optional: `username`, `hackatime_id`, `trust_verified`, `notes` (list of strings). `authenticated/me` carries `username`, `hackatime_id` and `trust_verified`; `trust_factor` and the stats body carry `trust_verified`, and `notes` when a breakdown is raw time. The `total_seconds=true` body has no extension beyond its two extra numbers.

**Trust.** `trust_level` and `trust_value` follow Hackatime's enum (`blue` 0, `red` 1, `green` 2, `yellow` 3). Responses to programs are masked as Hackatime masks them publicly (`yellow` reads as `blue`). A user with no Hackatime link, or a link whose level we couldn't read (`unknown`, after a rejected token), reads as `blue`, Hackatime's level for an account nobody has judged; `red` stays `red` once seen. Responses carrying a trust level add `logbook.trust_verified` (false for those two cases). The unmasked level is used only inside Logbook (ADR 0020, the admin API). Every level Logbook reads, when the user links Hackatime and on each trust refresh, is kept in `hackatime_trust_history` (one row per stretch of an unchanged level, its `checked_at` moved forward on each repeat; the admin API shows the newest 25).

| Call | Scope | Notes |
|---|---|---|
| `GET /api/v1/authenticated/me` | `profile` | Hackatime's shape; `id` is the numeric `public_id`. Adds the `logbook` object (above) |
| `GET /api/v1/authenticated/projects` | `read` | Hackatime's shape (`name`, `total_seconds`, `most_recent_heartbeat`, `languages`, `archived`). Adds `credited_seconds`, `raw_seconds`, `repo_url`. Hidden projects are left out |
| `GET /api/v1/users/{id}/stats` | public if the profile is public, else `read` from that user | Below |
| `GET /api/v1/users/{id}/trust_factor` | public | `{"trust_level", "trust_value"}`, masked (above) |
| `GET /api/v1/users/{id}/heartbeats/spans` | as stats | Hackatime's shape; raw heartbeats |
| `GET /api/v1/users/{id}/projects`, `GET /api/v1/users/{id}/project/{name}`, `GET /api/v1/users/{id}/projects/details` | as stats | Hackatime's shapes (`stats_controller.rb`); counted time unless `raw=true`, with `credited_seconds` and `raw_seconds` added |
| `GET /api/v1/my/heartbeats` | `read`, or the user's API key | Hackatime's shape |
| `GET /api/v1/submissions` | `submissions` (client credentials) | The program's submissions, as in `GET /api/v1/me/apps/{id}/submissions` |
| `GET /api/v1/submissions/{id}` | `submissions` | One submission with its packet and decision |

"As stats": public when the profile is public, else a credential of that same user (above). A private profile read by anyone else: `403 {"error": "user has disabled public stats"}`.

### `GET /api/v1/authenticated/me`

```json
{"id": 100000001, "emails": ["orpheus@example.com"], "slack_id": null, "github_username": "orpheus-gh",
 "trust_factor": {"trust_level": "green", "trust_value": 2},
 "logbook": {"username": "orpheus", "hackatime_id": 4242, "trust_verified": true}}
```

- `emails` is the user's HCA email (the `profile` scope shares it, as on Hackatime). `slack_id` is always `null`. `github_username` is the GitHub username Hackatime reported when the user linked it, else `null`.
- The trust level is always masked: Logbook has no "verified confidential apps" (Hackatime shows those the raw level).
- `logbook.hackatime_id` is left out when the user hasn't linked Hackatime.

### `GET /api/v1/authenticated/projects`

`{"projects": [...]}`, largest `total_seconds` first. Parameters as Hackatime's `ProjectStatsQuery`: `start`/`start_date` (default: all time), `end`/`end_date` (default: now; unparsable values fall back to the default), `projects` (comma list), `include_archived=true`; plus `raw=true`. A project is listed when it has raw or counted time in the range. `most_recent_heartbeat` is the last heartbeat; for a project the agent credited without editor heartbeats it is the end of the last credited minute. `languages` are raw, alphabetical.

### `GET /api/v1/users/{id}/projects`, `/project/{name}`, `/projects/details`

- `projects`: `{"projects": ["name", ...]}`, names with raw or counted time in the discovery range (`since`/`start`/`start_date`, default the start of the UTC day 30 days ago; `until`/`until_date`/`end`/`end_date`, default now), archived included, sorted by name.
- `project/{name}`: one row (below), or `404 {"error": "found nuthin"}`.
- `projects/details`: `{"projects": [row, ...]}`, filtered by `projects` (comma list); archived projects left out.
- Rows, in Hackatime's key order: `name`, `total_seconds` (counted unless `raw=true`), `languages`, `repo_url` (or `null`), `total_heartbeats`, `first_heartbeat`, `last_heartbeat`, `most_recent_heartbeat` (`YYYY-MM-DDTHH:MM:SSZ`), `archived`, then `credited_seconds`, `raw_seconds`. Range: `start`/`start_date` (default a year ago) to `end`/`end_date` (default now).

### `GET /api/v1/users/{id}/heartbeats/spans`

`{"spans": [{"start_time", "end_time", "duration"}]}`, Unix seconds, from raw heartbeats exactly as Hackatime's `to_span`: a span ends where the next heartbeat is more than 120 s away, and every span but the last gets that 120 s tail. Parameters: `start_date`/`end_date` (as stats; `422` when unparsable), `project` (one name, or `project[]=a&project[]=b`), else `filter_by_project` (comma list).

### `GET /api/v1/my/heartbeats`

`{"start_time", "end_time", "total_seconds", "heartbeats": [...]}` plus `credited_seconds` and `raw_seconds`. `start_time`/`end_time` default to the start and end of today (UTC) and render as `2026-06-10T00:00:00.000Z`. Heartbeats are oldest first, in Hackatime's model shape (`id`, `user_id` = public id, `time` in Unix seconds, `entity`, `type`, `category`, `project`, …, `source_type` `direct_entry` or `wakapi_import` for imported history; empty text fields are `null`). `total_seconds` is counted time unless `raw=true`. Differences: at most 25,000 heartbeats are listed (Hackatime has no cap), with `logbook.notes` saying so when cut; an unparsable time is `422 {"error": "Invalid start_time"}` where Hackatime fails with a `500`.

### Caching

Responses about a range that ended before the request (`users/{id}/*` with an `end_date`/`end` in the past) carry `Cache-Control: private, max-age=300`; everything else carries `no-store`. `trust_factor` and error responses carry no `Cache-Control`.

### `GET /api/v1/users/{id}/stats`

`{id}`: a username or user id. Parameters as on Hackatime: `start_date`, `end_date`, `features` (`projects`, `languages`, …), `filter_by_project` (comma list), `filter_by_category`, `total_seconds=true`, `no_ai_coding`, `test_param`, `limit`; plus ours:

- `raw=true`: `total_seconds` (and every per-item `total_seconds`) is time at the keyboard instead of counted time.

Rules:
- Without `raw=true`, the top-level `total_seconds` and every project's `total_seconds` are counted (credited) time from `credited_project_minutes`. Counted time is kept per project only, so **languages, editors, operating systems, machines and categories are always time at the keyboard**, from heartbeats, with or without `raw=true`. `logbook.notes` says so whenever such a breakdown is in the response. `filter_by_category` and `no_ai_coding` therefore change only raw figures.
- `total_seconds=true` returns `{"total_seconds": N, "credited_seconds": N, "raw_seconds": N}`.
- Otherwise the Hackatime body `{"data": {...}, "trust_factor": {...}, "logbook": {...}}`, with `data.credited_seconds` and `data.raw_seconds` added, and `credited_seconds`/`raw_seconds` on each project.
- A private profile without a `read` token from that user: Hackatime's `403` body.

Details that follow Hackatime exactly (pinned by the contract goldens):
- `start_date`/`end_date`: dates (`2025-12-15`, midnight UTC), RFC 3339 times or `YYYY-MM-DD HH:MM[:SS]` (UTC). Defaults: ten years ago and the end of today (UTC). Unparsable: `422 {"error": "Invalid start_date"}`. Bounds are truncated to whole seconds.
- `features`: a comma list; Hackatime builds only `languages` and `projects` (others are ignored). Absent means `languages`; present but empty means neither.
- `filter_by_project`: a comma list, or `filter_by_project[]=a&filter_by_project[]=b` (Rails splits an array on `,` elements; Stardance sends that).
- `test_param=true`: raw time leaves out the categories `browsing`, `meeting` and `communicating`. `no_ai_coding=true` also leaves out `ai coding` (with or without `test_param`).
- `filter_by_category` applies only to `total_seconds=true` without `test_param`, as on Hackatime (the summary path ignores it).
- `limit`: keep the first N rows of each breakdown; `0`, negative or absent means all.
- `data`: `username` (display name, else username), `user_id` (public id as a string), `is_coding_activity_visible`, `is_other_usage_visible`, `status: "ok"`, `start`, `end` (`YYYY-MM-DDTHH:MM:SSZ`), `range: "all_time"`, `human_readable_range: "All Time"`, `total_seconds`, `daily_average` (total divided by whole days in the range, integer), `human_readable_total` and `human_readable_daily_average` (`5h 10m 10s`, `""` for zero), then `languages` and/or `projects`, `unique_total_seconds` (when `features` mentions `projects` and `filter_by_project` is set), `streak` (the current streak from package `stats`: local days with at least 15 minutes).
- Breakdown rows: `name` (`Other` for an empty name), `total_seconds`, `text` (`5h 10m`), `hours`, `minutes`, `percent` (two decimals), `digital` (`HH:MM:SS`); languages add `color`, always Hackatime's default `#888888` (we don't ship GitHub's linguist colours); projects add `credited_seconds` and `raw_seconds`. Language rows and percentages are raw; project rows are counted (raw with `raw=true`), with percentages of the reported total, and include every project with raw or counted time.
- `unique_total_seconds`: without `raw=true`, the counted time of the filtered projects (already capped at 60 s per minute across projects, so nothing is counted twice); with `raw=true`, raw `coding` time with each minute counted once.

## Rate limits

Per token, or per IP for public calls: a burst of 600, refilled at 600 per minute. `/oauth/token`: per client, 60 per minute. Over the limit: `429` with `Retry-After`.

On the program API, a request whose credential verifies is limited by that credential (a bucket per token or API key); anything else, including a made-up token, by the client (one IPv4 address or IPv6 `/64`). One limiter covers all program API calls.
