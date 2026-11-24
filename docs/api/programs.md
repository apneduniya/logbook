# Programs, OAuth and the Hackatime-shaped program API

How a YSWS program connects to Logbook (ADR 0008, ADR 0021). Packages: `internal/server/oauth` (apps, grants, tokens), `internal/server/programs` (members), `internal/server/stats` (the read endpoints).

**Compatibility rule:** where a call exists on Hackatime, its path, parameters, status codes and JSON shape match Hackatime at commit `43bde10` exactly (`app/controllers/api/v1/stats_controller.rb`, `app/controllers/api/v1/authenticated/*`, `config/initializers/doorkeeper.rb`). Fields we add are extra keys that existing clients ignore. Contract tests in `internal/server/httpapi/testdata/hackatime/` pin each request a program repo makes (Stardance, Flavortown, Save Point, Sidekick, Crossroads, 3am-ysws) and its response shape.

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

Bearer token from OAuth, or none where marked public.

**User ids.** Hackatime ids are integers, and typed clients (including our own Hackatime client) decode `id` as one. Every user gets a numeric `public_id` (a `bigint` identity column, never reused), and every Hackatime-shaped response puts it in `id`. A path identifier is resolved as Hackatime does at `43bde10` (`StatsController#set_user`, `User.lookup_by_identifier`):
1. on the stats-controller endpoints, `my` is the user of the bearer token (which needs `read`); no other alias;
2. digits only: `public_id`, and if no user has it, a username equal to those digits;
3. otherwise a username (case-insensitive).

Compatibility exceptions, documented and covered by tests: Slack ids and HCA ids aren't accepted in V1 (HCA doesn't give us the Slack id), so Crossroads, which looks users up by Slack id, needs a small patch. `my` is reserved and can't be a username.

**Trust.** `trust_level` and `trust_value` follow Hackatime's enum (`blue` 0, `red` 1, `green` 2, `yellow` 3). Responses to programs are masked as Hackatime masks them publicly (`yellow` reads as `blue`). A user with no Hackatime link, or a link whose level we couldn't read (`unknown`, after a rejected token), reads as `blue`, Hackatime's level for an account nobody has judged; `red` stays `red` once seen. Responses add `logbook.trust_verified` (false for those two cases). The unmasked level is used only inside Logbook (ADR 0020, the admin API).

| Call | Scope | Notes |
|---|---|---|
| `GET /api/v1/authenticated/me` | `profile` | Hackatime's shape; `id` is the numeric `public_id`. Adds `username`, `hackatime_id` (or `null`), `logbook: true` |
| `GET /api/v1/authenticated/projects` | `read` | Hackatime's shape (`name`, `total_seconds`, `most_recent_heartbeat`, `languages`, `archived`). Adds `credited_seconds`, `raw_seconds`, `repo_url`. Hidden projects are left out |
| `GET /api/v1/users/{id}/stats` | public if the profile is public, else `read` from that user | Below |
| `GET /api/v1/users/{id}/trust_factor` | public | `{"trust_level", "trust_value"}`, masked (above) |
| `GET /api/v1/users/{id}/heartbeats/spans` | as stats | Hackatime's shape; raw heartbeats |
| `GET /api/v1/users/{id}/projects`, `GET /api/v1/users/{id}/project/{name}`, `GET /api/v1/users/{id}/projects/details` | as stats | Hackatime's shapes (`stats_controller.rb`); counted time unless `raw=true`, with `credited_seconds` and `raw_seconds` added |
| `GET /api/v1/my/heartbeats` | `read` | Hackatime's shape |
| `GET /api/v1/submissions` | `submissions` (client credentials) | The program's submissions, as in `GET /api/v1/me/apps/{id}/submissions` |
| `GET /api/v1/submissions/{id}` | `submissions` | One submission with its packet and decision |

### `GET /api/v1/users/{id}/stats`

`{id}`: a username or user id. Parameters as on Hackatime: `start_date`, `end_date`, `features` (`projects`, `languages`, …), `filter_by_project` (comma list), `filter_by_category`, `total_seconds=true`, `no_ai_coding`, `test_param`, `limit`; plus ours:

- `raw=true`: `total_seconds` (and every per-item `total_seconds`) is time at the keyboard instead of counted time.

Rules:
- Without `raw=true`, the top-level `total_seconds` and every project's `total_seconds` are counted (credited) time from `credited_project_minutes`. Counted time is kept per project only, so **languages, editors, operating systems, machines and categories are always time at the keyboard**, from heartbeats, with or without `raw=true`. `data.logbook.notes` says so whenever such a breakdown is in the response. `filter_by_category` and `no_ai_coding` therefore change only raw figures.
- `total_seconds=true` returns `{"total_seconds": N, "credited_seconds": N, "raw_seconds": N}`.
- Otherwise the Hackatime body `{"data": {...}, "trust_factor": {...}}`, with `data.credited_seconds` and `data.raw_seconds` added, and `credited_seconds`/`raw_seconds` on each project.
- A private profile without a `read` token from that user: Hackatime's `403` body.

## Rate limits

Per token, or per IP for public calls: a burst of 600, refilled at 600 per minute. `/oauth/token`: per client, 60 per minute. Over the limit: `429` with `Retry-After`.
