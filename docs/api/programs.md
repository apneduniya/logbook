# Programs, OAuth and the Hackatime-shaped program API

How a YSWS program connects to Logbook (ADR 0008, ADR 0021). Packages: `internal/server/oauth` (apps and their members, consent, codes, tokens, grants and the bearer middleware), `internal/server/programapi` (the Hackatime-shaped read endpoints, using `internal/server/stats` for streaks), `internal/server/programs` (the submissions addressed to a program; later step). Package `internal/server` wires them together: it adapts OAuth access tokens and user API keys to the program API's `Authenticator` and registers every route.

**Compatibility rule:** where a call exists on Hackatime, its path, parameters, status codes and JSON shape match Hackatime at commit `43bde10` exactly (`app/controllers/api/v1/stats_controller.rb`, `app/controllers/api/v1/authenticated/*`, `app/controllers/api/v1/my/heartbeats_controller.rb`, `app/services/project_stats_query.rb`, `lib/wakatime_service.rb`, `config/initializers/doorkeeper.rb`). Fields we add are extra keys that existing clients ignore. Contract tests in `internal/server/programapi/testdata/hackatime/` pin each request a program repo makes (Stardance, Flavortown, Save Point, Crossroads, Ari, the 11 project-picker repos) with its full response; run `go test ./internal/server/programapi -run Contract -update` to rewrite them after a deliberate change.

## Apps (programs)

Session auth (`RequireSession`: cookie only; bearer tokens and API keys get `401`); writes need CSRF. These are browser endpoints with the usual `/api/` CORS (auth.md). An app is also a program: its owner and reviewers are its **members** (ADR 0020 decisions).

| Call | What |
|---|---|
| `GET /api/v1/me/apps` | `{"apps": [App]}`: live apps the user owns or reviews for, newest first |
| `POST /api/v1/me/apps` | `{name, slug, redirect_uris[], homepage_url, public, confidential}`. `201` with the app plus `client_secret` (shown once; only SHA-256 is stored). `confidential` defaults to `true`; `false` makes a public client (no secret, PKCE required). Max 10 live apps per owner (`409`). Audited `app.create` |
| `GET /api/v1/me/apps/{id}` | Members. One app |
| `PATCH /api/v1/me/apps/{id}` | Owner only. Any of `name`, `redirect_uris`, `homepage_url`, `public`. `slug` and `confidential` can't change (`400`). Audited `app.update` |
| `POST /api/v1/me/apps/{id}/secret` | Owner only. Rotates the secret: `200` with the app plus the new `client_secret`, shown once. The old secret stops working at once; issued tokens stay valid. `409` for a public client. Audited `app.secret_rotate` |
| `DELETE /api/v1/me/apps/{id}` | Owner only. `204`. Revokes every token, unused code, pending request and grant of the app, and drops it from `/api/v1/programs`. Submissions keep the program's name but lose decisions-in-progress rights. Audited `app.delete` |
| `GET /api/v1/me/apps/{id}/members` | Members. `{"members": [{username, role, added_at, added_by}]}`, owner first |
| `POST /api/v1/me/apps/{id}/members` | Owner only. `{username}` (case-insensitive) becomes a `reviewer`: `201` with the member. `404` unknown user, `409` already a member or 50 reviewers already. Audited `app.member_add` |
| `DELETE /api/v1/me/apps/{id}/members/{username}` | Owner only. `204`. The owner can't be removed (`400`). Audited `app.member_remove` |
| `GET /api/v1/me/apps/{id}/submissions` | Members. Submissions addressed to the program, newest first, filter `status`, `decision` (step 5, submissions) |
| `GET /api/v1/programs` | No auth, limited per IP. `{"programs": [{slug, name, homepage_url}]}`: live public apps by name, for the submit picker |

`App`:
```json
{"id": "6f1c…", "client_id": "6f1c…", "name": "Stardance", "slug": "stardance",
 "redirect_uris": ["https://stardance.example/auth/hackatime/callback"], "homepage_url": "https://stardance.example",
 "public": true, "confidential": true, "role": "owner", "owner_username": "fiona-hackworth",
 "created_at": "2026-11-25T12:00:00Z", "updated_at": "2026-11-25T12:00:00Z", "secret_rotated_at": "2026-11-25T12:00:00Z"}
```

- `client_id` is the app's `id`.
- `name`: 1–64 characters, no control characters.
- `slug`: 2–40 of `a-z0-9-`, unique across all apps ever made: a deleted app keeps its slug, so nobody can take over a program's name.
- `redirect_uris`: 1–5, absolute, no fragment or user info; `https://`, or `http://localhost` / `http://127.0.0.1` with any port. Stored as given and matched exactly, character for character.
- `homepage_url`: optional, same scheme rules.
- Access: a user who isn't a member gets `404` for every `/api/v1/me/apps/{id}…` call (the response doesn't say whether the app exists); a reviewer gets `403` for owner-only writes. Nobody can add themselves to someone else's app, because only the owner adds members.

## OAuth 2

Our own provider (ADR 0021), shaped like Hackatime's Doorkeeper so existing OmniAuth and hand-written clients work unchanged.

**Secrets.** Client secrets (`lbs_…`), access tokens (`lbo_…`), refresh tokens (`lbr_…`), codes and consent request ids are 256 random bits, base64url. Only their SHA-256 is stored; secrets are compared in constant time.

**Scopes.** `profile` (the default) and `read`, granted by users; `submissions`, only on an app's own client-credentials token. Anything else is `invalid_scope`.

### `GET /oauth/authorize`

On the API origin; no CORS. Query: `client_id`, `redirect_uri` (required), `response_type=code`, `scope` (space-separated; default `profile`), `state` (returned as is, up to 2048 bytes), optional `code_challenge` + `code_challenge_method=S256` (required when the app has no secret). Limited to 120 a minute per IP.

- Unknown or deleted client, or a `redirect_uri` that isn't exactly one of the app's: `302` to the web UI's error page `{LOGBOOK_WEB_URL}/oauth/authorize?error=invalid_client` (or `invalid_redirect_uri`). Never to the unverified URI.
- Other errors: `302` to `redirect_uri` with `error`, `error_description` and `state` (RFC 6749 §4.1.2.1): `unsupported_response_type`, `invalid_scope`, or `invalid_request` (a public client without `code_challenge`; a method other than `S256`, including a challenge without a method, which RFC 7636 reads as `plain`; a malformed challenge; `state` over 2048 bytes).
- If the browser has a session whose user already granted at least these scopes to this app, the code is issued straight away (`302` to `redirect_uri?code=…&state=…`), as Doorkeeper does.
- Otherwise `302` to `{LOGBOOK_WEB_URL}/oauth/authorize?request=<id>`: a 10-minute pending request, stored server side. If the user is signed out, the web UI sends them to sign in first. A request started with a session belongs to that user; one started signed out belongs to the first signed-in user who opens it. Nobody else can read or decide it (`404`).

### `GET /api/v1/oauth/requests/{id}`

Session. The pending request for the consent page:
```json
{"app": {"id": "6f1c…", "name": "Stardance", "slug": "stardance", "homepage_url": "https://stardance.example", "owner_username": "fiona-hackworth"},
 "scopes": [{"name": "profile", "description": "See your basic profile: …"}, {"name": "read", "description": "See your coding activity: …"}],
 "redirect_uri": "https://stardance.example/auth/hackatime/callback", "already_granted": false, "expires_at": "2026-11-25T12:10:00Z"}
```
`already_granted` is true when the user granted these scopes to the app before (for example, they signed in after starting); the page may then approve without asking. `404` for unknown, expired, decided or other users' requests, and when the app was deleted or no longer registers the request's `redirect_uri` (the owner removed it after the request started).

### `POST /api/v1/oauth/requests/{id}`

Session + CSRF. `{"approve": true|false}` (required). Returns `{"redirect": "<redirect_uri>?code=…&state=…"}` or, when denied, `{"redirect": "<redirect_uri>?error=access_denied&error_description=…&state=…"}`. Single use (`404` after). Approving records the grant (its scopes add to any granted before) and is audited `oauth.grant`. `404`, with nothing recorded, when the app was deleted or no longer registers the request's `redirect_uri`: the browser is never sent, even with `access_denied`, to a URI the app doesn't register now.

Codes are single use, valid 10 minutes, and bound to the app, user, redirect URI, scopes and PKCE challenge.

### `POST /oauth/token`

No CORS. Form-encoded; a flat JSON object is also accepted, as Doorkeeper (Rails) does. Query parameters are ignored. Client authentication by HTTP Basic (`client_id:client_secret`, each form-encoded per RFC 6749 §2.3.1) or `client_id`/`client_secret` in the body; when an `Authorization` header is present it is the only one read. A public client sends only `client_id` and must not send a secret.

- `grant_type=authorization_code`: `code`, `redirect_uri` (exactly the one used to authorize; also still registered), `code_verifier` when a challenge was sent. Any attempt by the code's own client uses the code up, right or wrong. Presenting a used code again revokes every token issued from it and their refreshes (RFC 6749 §4.1.2). A `code_verifier` for a code that had no challenge is refused. Another client's attempt fails without using the code.
- `grant_type=refresh_token`: `refresh_token`, optional `scope` (the same or fewer scopes). Returns a new access and refresh token and revokes the old pair. Presenting a rotated refresh token again revokes the whole family, even while another refresh of the family is in flight. Refresh tokens last 2 years from issue.

Used codes and rotated refresh tokens are kept for as long as any token issued from the same code is still usable, so a replay at any point in the family's life revokes it. Once nothing in the family is usable, a code is deleted 24 hours after it expired and a token 30 days after it expired or was revoked.
- `grant_type=client_credentials`: confidential apps only (`unauthorized_client` otherwise). `scope` empty or `submissions`. The token acts as the app, has no refresh token and lasts 1 hour (ADR 0021 doesn't set it; a short life costs the program nothing, because it can always ask again).

A user grant that was revoked, or an app that was deleted, makes every grant type fail.

`200` (Doorkeeper's shape; `Cache-Control: no-store`):
```json
{"access_token": "lbo_…", "token_type": "Bearer", "expires_in": 31536000, "refresh_token": "lbr_…", "scope": "profile read", "created_at": 1795000000}
```
Errors per RFC 6749 §5.2, as `{"error": "…", "error_description": "…"}`: `invalid_request`, `invalid_grant`, `invalid_scope`, `unauthorized_client`, `unsupported_grant_type` with `400`; `invalid_client` with `401` (plus `WWW-Authenticate: Basic` when Basic was used); `server_error` with `500`.

### `POST /oauth/revoke`

RFC 7009. No CORS. Same body and client authentication as `/oauth/token`, with `token` (and an optional `token_type_hint`, ignored). Bad client authentication is `401 invalid_client`; otherwise always `200 {}`. Only a token issued to the calling client is revoked, and revoking either the access or the refresh token revokes both. Audited `oauth.revoke` when a token was revoked.

### User grants

Session; `DELETE` needs CSRF. Browser endpoints with the usual `/api/` CORS.

- `GET /api/v1/me/grants`: `{"grants": [{app_id, app_name, app_slug, homepage_url, owner_username, scopes, created_at, updated_at}]}`, the user's active grants to live apps, newest first.
- `DELETE /api/v1/me/grants/{app_id}`: `204`. Every token and unused code the app holds for this user stops working at once, and the next authorize asks again. A code exchange or refresh running at the same moment is ordered against the revoke (the exchange locks the code, the refresh locks the grant), so a token it issues is revoked too. `404` if there is no active grant. Audited `oauth.revoke`.

### Bearer tokens

An access token is sent as `Authorization: Bearer lbo_…` (the only form accepted, as Doorkeeper's `from_bearer_authorization`; never in the query string). Refresh tokens and client secrets are never accepted as one, and OAuth tokens are never accepted by session or API-key endpoints (the web UI's `/api/v1/me/*`, ingest, the agent's routes): those answer `401`.

- **The program API** (below) answers in Hackatime's shapes: on `authenticated/*` a missing or invalid token (unknown, expired, revoked, of a revoked grant, or of a deleted app) is `401` and a token without the call's scope `403`, both with an empty body and Doorkeeper's `WWW-Authenticate: Bearer realm="Doorkeeper", error="invalid_token"` (or `"insufficient_scope"`); on `users/{id}/*` a token that doesn't verify or lacks `read` is ignored. A client-credentials token has no user, so the program API treats it as invalid: `401` on `authenticated/*` and `my/heartbeats`, anonymous on `users/{id}/*`.
- **Logbook's own bearer endpoints** (the program's submission calls, a later step) use `oauth.RequireScope`: `401` with `WWW-Authenticate: Bearer realm="logbook", error="invalid_token"`, or `403` with `error="insufficient_scope"`, each with a JSON `{"error": "…"}` body. A client-credentials token only ever passes `submissions`.

The server (`internal/server/router`) hands the program API an adapter over `oauth.Service.AuthenticateBearer` and `auth.Service.APIKeyOwner`; `internal/server/router/e2e_test.go` runs the whole flow (register, consent, exchange, read, revoke, client credentials) on the real handler.

## The program API

Bearer token from OAuth, or none where marked public. Package `internal/server/programapi`; the server (`internal/server/router`) injects an `Authenticator` that resolves OAuth access tokens a user granted (package `oauth`; client-credentials tokens resolve to nothing) and user API keys (package `auth`).

**No CORS.** Programs call this API from their servers, and Hackatime's open CORS is not carried over: `authenticated/*`, `my/*` and the `users/{id}/` calls below (`stats`, `trust_factor`, `heartbeats/*`, `projects*`, `project/*`) answer no CORS, preflights included, so a browser page on any origin, ours included, can't read them. The web UI's own `users/{username}/profile` and `/live` keep CORS for the web origin (auth.md).

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
- `start_date`/`end_date`: dates (`2025-12-15`, midnight UTC), RFC 3339 times or `YYYY-MM-DD HH:MM[:SS]` (UTC). An offset may also be written without a colon (`+0530`) or after a space, and a trailing ` UTC` is accepted, so Ruby's `Time#to_s` (`2026-06-10 10:00:00 UTC`, `2026-06-10 10:00:00 +0000`), which Ruby clients send when they put a `Time` in a query, parses as on Hackatime. The same forms apply to every time parameter on the program API. Defaults: ten years ago and the end of today (UTC). Unparsable: `422 {"error": "Invalid start_date"}`. Bounds are truncated to whole seconds.
- `features`: a comma list; Hackatime builds only `languages` and `projects` (others are ignored). Absent means `languages`; present but empty means neither.
- `filter_by_project`: a comma list, or `filter_by_project[]=a&filter_by_project[]=b` (Rails splits an array on `,` elements; Stardance sends that). Blank or absent means no filter. A value of only commas (`filter_by_project=,`) is an empty filter that matches nothing, as Ruby's `",".split(",")` is `[]`: totals are `0`, `projects` is empty, and `features=projects` still adds `unique_total_seconds: 0`. The same holds for `filter_by_category` and for spans' `filter_by_project`.
- `test_param=true`: raw time leaves out the categories `browsing`, `meeting` and `communicating`. `no_ai_coding=true` also leaves out `ai coding` (with or without `test_param`).
- `filter_by_category` applies only to `total_seconds=true` without `test_param`, as on Hackatime (the summary path ignores it).
- `limit`: keep the first N rows of each breakdown; `0`, negative or absent means all.
- `data`: `username` (display name, else username), `user_id` (public id as a string), `is_coding_activity_visible`, `is_other_usage_visible`, `status: "ok"`, `start`, `end` (`YYYY-MM-DDTHH:MM:SSZ`), `range: "all_time"`, `human_readable_range: "All Time"`, `total_seconds`, `daily_average` (total divided by whole days in the range, integer), `human_readable_total` and `human_readable_daily_average` (`5h 10m 10s`, `""` for zero), then `languages` and/or `projects`, `unique_total_seconds` (when `features` mentions `projects` and `filter_by_project` is set), `streak` (the current streak from package `stats`: local days with at least 15 minutes).
- Breakdown rows: `name` (`Other` for an empty name), `total_seconds`, `text` (`5h 10m`), `hours`, `minutes`, `percent` (two decimals), `digital` (`HH:MM:SS`); languages add `color`, always Hackatime's default `#888888` (we don't ship GitHub's linguist colours); projects add `credited_seconds` and `raw_seconds`. Language rows and percentages are raw; project rows are counted (raw with `raw=true`), with percentages of the reported total, and include every project with raw or counted time.
- `unique_total_seconds`: without `raw=true`, the counted time of the filtered projects (already capped at 60 s per minute across projects, so nothing is counted twice); with `raw=true`, raw `coding` time with each minute counted once.

## Rate limits

Per token, or per IP for public calls: a burst of 600, refilled at 600 per minute. `/oauth/token` and `/oauth/revoke` share a budget of 60 per minute per client and caller network (IPv4 address or IPv6 `/64`): the client id is public, so keying on it alone would let anyone use up a program's budget. `/oauth/authorize`: 120 per minute per IP. `/api/v1/programs`: the public budget of 120 per minute per IP. Over the limit: `429` with `Retry-After` and `{"error": "too many requests"}`.

On the program API, a request whose credential verifies is limited by that credential (a bucket per token or API key); anything else, including a made-up token, by the client (one IPv4 address or IPv6 `/64`). One limiter covers all program API calls.
