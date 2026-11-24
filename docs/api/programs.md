# Programs, OAuth and the Hackatime-shaped program API

How a YSWS program connects to Logbook (ADR 0008, ADR 0021). Packages: `internal/server/oauth` (apps and their members, consent, codes, tokens, grants and the bearer middleware), `internal/server/programs` (the submissions addressed to a program), `internal/server/stats` (the read endpoints).

**Compatibility rule:** where a call exists on Hackatime, its path, parameters, status codes and JSON shape match Hackatime at commit `43bde10` exactly (`app/controllers/api/v1/stats_controller.rb`, `app/controllers/api/v1/authenticated/*`, `config/initializers/doorkeeper.rb`). Fields we add are extra keys that existing clients ignore. Contract tests in `internal/server/httpapi/testdata/hackatime/` pin each request a program repo makes (Stardance, Flavortown, Save Point, Sidekick, Crossroads, 3am-ysws) and its response shape.

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
`already_granted` is true when the user granted these scopes to the app before (for example, they signed in after starting); the page may then approve without asking. `404` for unknown, expired, decided or other users' requests.

### `POST /api/v1/oauth/requests/{id}`

Session + CSRF. `{"approve": true|false}` (required). Returns `{"redirect": "<redirect_uri>?code=…&state=…"}` or, when denied, `{"redirect": "<redirect_uri>?error=access_denied&error_description=…&state=…"}`. Single use (`404` after). Approving records the grant (its scopes add to any granted before) and is audited `oauth.grant`.

Codes are single use, valid 10 minutes, and bound to the app, user, redirect URI, scopes and PKCE challenge.

### `POST /oauth/token`

No CORS. Form-encoded; a flat JSON object is also accepted, as Doorkeeper (Rails) does. Query parameters are ignored. Client authentication by HTTP Basic (`client_id:client_secret`, each form-encoded per RFC 6749 §2.3.1) or `client_id`/`client_secret` in the body; when an `Authorization` header is present it is the only one read. A public client sends only `client_id` and must not send a secret.

- `grant_type=authorization_code`: `code`, `redirect_uri` (exactly the one used to authorize; also still registered), `code_verifier` when a challenge was sent. Any attempt by the code's own client uses the code up, right or wrong. Presenting a used code again revokes every token issued from it and their refreshes (RFC 6749 §4.1.2). A `code_verifier` for a code that had no challenge is refused. Another client's attempt fails without using the code.
- `grant_type=refresh_token`: `refresh_token`, optional `scope` (the same or fewer scopes). Returns a new access and refresh token and revokes the old pair. Presenting a rotated refresh token again revokes the whole family. Refresh tokens last 2 years from issue.
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
- `DELETE /api/v1/me/grants/{app_id}`: `204`. Every token and unused code the app holds for this user stops working at once, and the next authorize asks again. `404` if there is no active grant. Audited `oauth.revoke`.

### Bearer tokens

Program API calls send `Authorization: Bearer lbo_…` (the only form accepted, as Doorkeeper's `from_bearer_authorization`). A missing or invalid token (unknown, expired, revoked, or of a deleted app) is `401` with `WWW-Authenticate: Bearer realm="logbook", error="invalid_token"`; a token without the endpoint's scope is `403` with `error="insufficient_scope"`. Both carry a JSON `{"error": "…"}` body. Sessions, user API keys, refresh tokens and client secrets are never accepted, and OAuth tokens are never accepted by session or API-key endpoints. A client-credentials token only ever passes `submissions`.

## The program API

Bearer token from OAuth, or none where marked public.

**User ids.** Hackatime ids are integers, and typed clients (including our own Hackatime client) decode `id` as one. Every user gets a numeric `public_id` (a `bigint` identity column, never reused), and every Hackatime-shaped response puts it in `id`. A path identifier is resolved as Hackatime does at `43bde10` (`StatsController#set_user`, `User.lookup_by_identifier`):
1. on the stats-controller endpoints, `my` is the user of the bearer token (which needs `read`); no other alias;
2. digits only: `public_id`, and if no user has it, a username equal to those digits;
3. otherwise a username (case-insensitive).

Compatibility exceptions, documented and covered by tests: Slack ids and HCA ids aren't accepted in V1 (HCA doesn't give us the Slack id), so Crossroads, which looks users up by Slack id, needs a small patch. `my` is reserved and can't be a username.

**The `logbook` extension.** Any Hackatime-shaped response may carry one extra top-level object, `logbook`, which Hackatime clients ignore. Its fields, all optional: `username`, `hackatime_id`, `trust_verified`, `notes` (list of strings). `authenticated/me` carries `username`, `hackatime_id` and `trust_verified`; `trust_factor` and the stats body carry `trust_verified`, and `notes` when a breakdown is raw time. The `total_seconds=true` body has no extension beyond its two extra numbers.

**Trust.** `trust_level` and `trust_value` follow Hackatime's enum (`blue` 0, `red` 1, `green` 2, `yellow` 3). Responses to programs are masked as Hackatime masks them publicly (`yellow` reads as `blue`). A user with no Hackatime link, or a link whose level we couldn't read (`unknown`, after a rejected token), reads as `blue`, Hackatime's level for an account nobody has judged; `red` stays `red` once seen. Responses carrying a trust level add `logbook.trust_verified` (false for those two cases). The unmasked level is used only inside Logbook (ADR 0020, the admin API).

| Call | Scope | Notes |
|---|---|---|
| `GET /api/v1/authenticated/me` | `profile` | Hackatime's shape; `id` is the numeric `public_id`. Adds the `logbook` object (above) |
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
- Without `raw=true`, the top-level `total_seconds` and every project's `total_seconds` are counted (credited) time from `credited_project_minutes`. Counted time is kept per project only, so **languages, editors, operating systems, machines and categories are always time at the keyboard**, from heartbeats, with or without `raw=true`. `logbook.notes` says so whenever such a breakdown is in the response. `filter_by_category` and `no_ai_coding` therefore change only raw figures.
- `total_seconds=true` returns `{"total_seconds": N, "credited_seconds": N, "raw_seconds": N}`.
- Otherwise the Hackatime body `{"data": {...}, "trust_factor": {...}}`, with `data.credited_seconds` and `data.raw_seconds` added, and `credited_seconds`/`raw_seconds` on each project.
- A private profile without a `read` token from that user: Hackatime's `403` body.

## Rate limits

Per token, or per IP for public calls: a burst of 600, refilled at 600 per minute. `/oauth/token` and `/oauth/revoke` share a budget of 60 per minute per client and caller network (IPv4 address or IPv6 `/64`): the client id is public, so keying on it alone would let anyone use up a program's budget. `/oauth/authorize`: 120 per minute per IP. Over the limit: `429` with `Retry-After` and `{"error": "too many requests"}`.
