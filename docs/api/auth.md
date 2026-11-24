# Auth API

Owned by `internal/server/auth`. Covers sign-in with Hack Club Auth (HCA), browser sessions, CSRF, API keys and the localhost-only dev sign-in.

## Conventions

- Errors are JSON: `{"error": "message"}`.
- **Session auth:** the browser sends the `logbook_session` cookie (`HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`). The UI can't read it. Sessions slide: any request made more than 5 minutes after the last refresh extends the expiry to now + `LOGBOOK_SESSION_TTL` (default 30 days) and re-issues the cookie.
- **CSRF:** every session-authenticated request whose method isn't `GET`, `HEAD` or `OPTIONS` must send `X-CSRF-Token: <csrf_token>`. Get the token from `GET /api/v1/session`. It is fixed for the life of the session. A missing or wrong token gets `403 {"error": "missing or invalid CSRF token"}`.
- **No session** gets `401 {"error": "not signed in"}`.
- **Rate limits:** limits are per client, where a client is one IPv4 address or one IPv6 `/64` network. Budgets are sized so a school or hackathon behind one NAT address can sign in together. Each is a burst of N, refilled at N per minute:
  - `GET /auth/login` and `POST /auth/dev-login` share 120.
  - `GET /auth/callback` and `POST /auth/logout` share 300. This is a separate budget, so a sign-in that has started can still finish.
  - Failed API key attempts: 60 (see below). Successful API key requests are never limited here.

  Over a limit the response is `429` with a `Retry-After` header (seconds).

## Web origin and CORS

The web UI and the API live on different origins (ADR 0011): the UI at `LOGBOOK_WEB_URL` (for example `https://logbook.boogles.dev`), the API at `LOGBOOK_BASE_URL` (for example `https://api.logbook.boogles.dev`). OAuth callbacks are on the API origin.

- `LOGBOOK_WEB_URL` must be an origin: `https://` plus a host and optional port, with no path, query or fragment. A default port written out (`:443`, or `:80` for `http`) is refused, because browsers leave it out of `Origin` and CORS would never match. CORS and sign-in redirects build the origin with the same function (`config.ParseOrigin`). Plain `http` is accepted only for `localhost`, `127.0.0.1` or `::1` with `LOGBOOK_DEV_INSECURE_COOKIE=true`, and it then defaults to `http://localhost:5173`. Otherwise it is required.
- After sign-in, the API redirects the browser to `LOGBOOK_WEB_URL` plus the saved `next` path. Only that origin is ever used, so `next` can't send anyone elsewhere.

### CORS

For paths under `/api/` the API answers CORS for exactly the web origin:

- Preflight (`OPTIONS` with `Access-Control-Request-Method`): `204` with `Access-Control-Allow-Origin: <web origin>`, `Access-Control-Allow-Credentials: true`, `Access-Control-Allow-Methods: GET, POST, PATCH, DELETE`, `Access-Control-Allow-Headers: Content-Type, X-CSRF-Token` and `Access-Control-Max-Age: 600`. Any other origin, method or header gets `403`.
- Actual requests from the web origin get `Access-Control-Allow-Origin`, `Access-Control-Allow-Credentials: true` and `Access-Control-Expose-Headers: Retry-After, X-Request-Id`. Requests from other origins are served without CORS headers, so the browser hides the response from the page.
- Every `/api/` response carries `Vary: Origin`.
- There is no CORS on `/auth/*`, `/healthz`, heartbeat ingest (`/api/v1/users/current/heartbeats*`, `/api/v1/users/current/statusbar/*`, `/api/hackatime/*`) or the agent's device endpoints (`/api/v1/devices*`). Browsers never call them cross-origin.
- CSRF protection is unchanged: state-changing session requests still need `X-CSRF-Token`, which a page on another origin can't send without passing CORS.

## Browser sign-in (HCA)

### `GET /auth/login`

Starts sign-in. The UI navigates the browser here; don't call it with `fetch`.

| | |
|---|---|
| Auth | none |
| Query | `next` (optional): a same-origin path such as `/projects?range=week` to land on afterwards. Anything else, including absolute URLs, `//host`, backslashes and control characters, is replaced by `/`. |
| Response | `302` to `https://auth.hackclub.com/oauth/authorize` with `state`, `code_challenge` (S256) and `scope=email name`. Sets a 10-minute `logbook_oauth` cookie (`Path=/auth/`, `HttpOnly`) that holds the state, PKCE verifier and `next`, MAC'd with a key derived from the server key. |
| Errors | `503` if HCA isn't configured (`LOGBOOK_HCA_CLIENT_ID` unset). `429` if rate-limited. |

### `GET /auth/callback`

HCA redirects here. Not for direct use.

| | |
|---|---|
| Auth | the `logbook_oauth` cookie from `/auth/login` |
| Query | `code`, `state` (or `error` from HCA) |
| Success | Creates the user on first sign-in, refreshes name and email on later ones, replaces any existing session on this browser with a new one, writes `auth.login` to the audit log, sets `logbook_session`, and responds `302` to `LOGBOOK_WEB_URL` plus the saved `next` path (default `/`). |
| Failure | `302` to `LOGBOOK_WEB_URL` + `/?auth_error=<code>`, with no session: |

| `auth_error` | Meaning |
|---|---|
| `invalid_state` | Missing, expired (over 10 min), tampered or mismatched state. Ask the user to try again. |
| `access_denied` | The user declined, or HCA returned an error. |
| `failed` | Code exchange or identity fetch failed (including a PKCE mismatch). |

Every callback clears the state cookie in the browser. The state itself is not recorded on the server: it is a MAC'd cookie that expires after 10 minutes. A replayed callback fails because HCA codes are single use and bound to the PKCE verifier.

**New users:** the username comes from the HCA first and last name (`Fiona Hackworth` becomes `fiona-hackworth`). Names already taken get a numeric suffix (`fiona-hackworth2`). If the name isn't usable (for example it has no Latin letters, or the name scope wasn't granted), the username is `hacker` plus six random digits (`hacker402913`). The email address is never used, because usernames are public. The username never changes on later sign-ins.

**Reserved username:** `my` (in any case) can't be a username: the program API reads `/api/v1/users/my/...` as the token's own user (ADR 0021). A name that sanitises to `my` gets the next candidate (`my2`). The database refuses it too (`users_username_not_reserved`).

**Public id:** every user also has a numeric `public_id` (from `100000001` up, never reused), which Hackatime-shaped responses use as `id` (see [programs.md](programs.md#the-program-api)). It isn't shown on `GET /api/v1/session`.

### `POST /auth/logout`

| | |
|---|---|
| Auth | session + CSRF |
| Request | no body |
| Response | `204`. Deletes the session, clears the cookie and writes `auth.logout` to the audit log. |
| Errors | `401` with no session, `403` with a bad CSRF token, `429` if rate-limited. |

### `POST /auth/dev-login` (development only)

This route exists only when `LOGBOOK_DEV_LOGIN=true` **and** `LOGBOOK_BASE_URL` is on `localhost`, `127.0.0.1` or `::1`. Config loading refuses `LOGBOOK_DEV_LOGIN=true` on any other host, and also when `LOGBOOK_ADDR` listens on anything other than loopback (for example `0.0.0.0:8080` or `:8080`). The handler also answers only connections whose TCP peer is a loopback address; `X-Forwarded-For` is ignored. Otherwise it returns `404`.

| | |
|---|---|
| Auth | none |
| Request | Either JSON or a form. **JSON** (`Content-Type: application/json`, for scripts): `{"username": "orpheus"}`. A cross-site page can't send it without a CORS preflight, and `/auth/*` answers none. **Form** (`application/x-www-form-urlencoded`, for the web UI on its own origin): `username` and an optional `next` path. Forms can be posted from any site, so the request's `Origin` header must be exactly the web origin. The username is lowercased and must match `[a-z0-9_-]{2,32}`. |
| Response | JSON: `200`, the same body as `GET /api/v1/session`, plus the `logbook_session` cookie. Form: `303` to `LOGBOOK_WEB_URL` plus the safe `next` path (default `/`), plus the cookie. The user is created on first use with `hca_id = "dev:<username>"`. |
| Errors | `404` disabled or not from loopback, `403` form from any origin other than the web origin, `415` other content types, `400` invalid or reserved (`my`) username, JSON or form, `429` rate-limited. |

## Session

### `GET /api/v1/session`

The signed-in user and the CSRF token. The UI calls this on load to decide whether to show the sign-in button.

| | |
|---|---|
| Auth | session |
| Response | `200`, `Cache-Control: no-store` |
| Errors | `401` with no valid session |

```json
{
  "user": {
    "id": "6f1c…",
    "username": "fiona-hackworth",
    "display_name": "Fiona Hackworth",
    "email": "fiona@example.com",
    "avatar_url": "",
    "timezone": "UTC",
    "profile_public": true,
    "role": "user",
    "created_at": "2026-11-20T12:00:00Z"
  },
  "csrf_token": "b64url…",
  "expires_at": "2026-12-20T12:00:00Z"
}
```

### `DELETE /api/v1/session`

Signs out, exactly like `POST /auth/logout` (same handler, rate limit and audit entry). The web UI uses this route: it lives on another origin, and CORS covers `/api/` only, so a cross-origin `POST /auth/logout` with the CSRF header would fail its preflight.

| | |
|---|---|
| Auth | session + CSRF |
| Response | `204`. Deletes the session, clears the cookie and writes `auth.logout` to the audit log. |
| Errors | `401` with no session, `403` with a bad CSRF token, `429` if rate-limited (shares the `/auth/callback` budget). |

## API keys

API keys are for editor plugins (through stock `wakatime-cli`) and the Logbook agent. A key is a random lowercase UUIDv4 (`xxxxxxxx-xxxx-4xxx-[89ab]xxx-xxxxxxxxxxxx`), the format `wakatime-cli` accepts. The server stores only its SHA-256 and the first 8 characters (`prefix`), so it can show the key only once, at creation. Each user can have at most **20** active keys. Creating or revoking a key writes `api_key.create` / `api_key.revoke` to the audit log, with the name and prefix but never the key.

Only session auth can manage keys. A request authenticated with an API key gets `401`.

### `GET /api/v1/api_keys`

| | |
|---|---|
| Auth | session |
| Response | `200`, active keys only, newest first |

```json
{
  "api_keys": [
    {"id": "0b6e…", "name": "laptop", "prefix": "3f1e2d4c",
     "created_at": "2026-11-20T12:00:00Z", "last_used_at": null}
  ]
}
```

`last_used_at` is updated at most once a minute per key.

### `POST /api/v1/api_keys`

| | |
|---|---|
| Auth | session + CSRF |
| Request | `{"name": "laptop"}`. Leading and trailing spaces are trimmed. The result must be 1–64 bytes with no control characters. |
| Response | `201`, `Cache-Control: no-store`. The only time `key` is returned. |
| Errors | `400` invalid name or JSON, `409` already at 20 active keys, `401`, `403` |

```json
{"id": "0b6e…", "name": "laptop", "prefix": "3f1e2d4c",
 "created_at": "2026-11-20T12:00:00Z", "last_used_at": null,
 "key": "3f1e2d4c-1111-4222-8333-444455556666"}
```

### `DELETE /api/v1/api_keys/{id}`

| | |
|---|---|
| Auth | session + CSRF |
| Response | `204`. The key stops working at once. |
| Errors | `404` if the key doesn't exist, is already revoked, or belongs to another user (the response doesn't say which). `401`, `403`. |

## Authenticating with an API key (plugins, agent)

Routes guarded by `RequireAPIKey` (WakaTime-compatible ingest, agent endpoints) accept a key in any of these forms, checked in this order:

1. `Authorization: Bearer <key>`
2. `Authorization: Basic <base64(key)>`, which is what `wakatime-cli` sends. `base64("<key>:")` and `base64(":<key>")` also work.
3. `?api_key=<key>` query parameter

If an `Authorization` header is present, it is the only form checked. A bad key, a malformed key, a revoked key or no key gets `401 {"error": "invalid API key"}`. Each client (IPv4 address or IPv6 `/64`) may make 60 failed attempts in a burst, refilled at 60 per minute. Past that, further **failed** attempts from that client get `429` with `Retry-After` instead of `401`. A request with a valid key always succeeds, even from a client that is over its failure budget, so one misconfigured editor on a shared network can't block everyone else's heartbeats.

Behind a reverse proxy, set `LOGBOOK_TRUST_PROXY=true` so rate limits use the last `X-Forwarded-For` hop instead of the proxy's address.

The program API also takes a user API key where Hackatime takes its own: `users/{id}/*` (`Bearer` only) and `my/heartbeats` (`Bearer` or `Basic base64(key)`). It looks keys up through `auth.Service.APIKeyOwner` and applies its own rate limit (see [programs.md](programs.md#the-program-api)); `authenticated/*` never accepts an API key.
