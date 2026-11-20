# Auth API

Owned by `internal/server/auth`. Covers sign-in with Hack Club Auth (HCA), browser sessions, CSRF, API keys and the localhost-only dev sign-in.

## Conventions

- Errors are JSON: `{"error": "message"}`.
- **Session auth:** the browser sends the `logbook_session` cookie (`HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`). The UI can't read it. Sessions slide: any request made more than 5 minutes after the last refresh extends the expiry to now + `LOGBOOK_SESSION_TTL` (default 30 days) and re-issues the cookie.
- **CSRF:** every session-authenticated request whose method isn't `GET`, `HEAD` or `OPTIONS` must send `X-CSRF-Token: <csrf_token>`. Get the token from `GET /api/v1/session`. It is fixed for the life of the session. A missing or wrong token gets `403 {"error": "missing or invalid CSRF token"}`.
- **No session** gets `401 {"error": "not signed in"}`.
- **Rate limits:** each client IP can make 20 requests to `/auth/*` in a burst, refilled at 20 per minute. Over the limit the response is `429` with a `Retry-After` header (seconds).

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
| Success | Creates the user on first sign-in, refreshes name and email on later ones, replaces any existing session on this browser with a new one, writes `auth.login` to the audit log, sets `logbook_session`, and responds `302` to the saved `next` path (default `/`). |
| Failure | `302` to `/?auth_error=<code>`, with no session: |

| `auth_error` | Meaning |
|---|---|
| `invalid_state` | Missing, expired (over 10 min), tampered or mismatched state. Ask the user to try again. |
| `access_denied` | The user declined, or HCA returned an error. |
| `failed` | Code exchange or identity fetch failed (including a PKCE mismatch). |

The state cookie is cleared on every callback, so each login attempt can be used once.

**New users:** the username comes from the HCA first and last name (`Fiona Hackworth` becomes `fiona-hackworth`). If the name isn't usable, it comes from the email's local part, and failing that it is `hacker`. Names already taken get a numeric suffix (`fiona-hackworth2`). The username never changes on later sign-ins.

### `POST /auth/logout`

| | |
|---|---|
| Auth | session + CSRF |
| Request | no body |
| Response | `204`. Deletes the session, clears the cookie and writes `auth.logout` to the audit log. |
| Errors | `401` with no session, `403` with a bad CSRF token, `429` if rate-limited. |

### `POST /auth/dev-login` (development only)

This route exists only when `LOGBOOK_DEV_LOGIN=true` **and** `LOGBOOK_BASE_URL` is on `localhost`, `127.0.0.1` or `::1`. Config loading refuses `LOGBOOK_DEV_LOGIN=true` on any other host. When the route is disabled it returns `404`.

| | |
|---|---|
| Auth | none |
| Request | `Content-Type: application/json` (required, which blocks cross-site form posts). Body: `{"username": "orpheus"}`. The username is lowercased and must match `[a-z0-9_-]{2,32}`. |
| Response | `200`, the same body as `GET /api/v1/session`, plus the `logbook_session` cookie. The user is created on first use with `hca_id = "dev:<username>"`. |
| Errors | `404` disabled, `415` wrong content type, `400` invalid username or JSON, `429` rate-limited. |

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

If an `Authorization` header is present, it is the only form checked. A bad key, a revoked key or no key gets `401 {"error": "invalid API key"}`. Each IP may make 10 failed attempts in a burst, refilled at 10 per minute. Past that, every API-key request from that IP gets `429` with `Retry-After` until the budget refills, even if it carries a valid key.

Behind a reverse proxy, set `LOGBOOK_TRUST_PROXY=true` so rate limits use the last `X-Forwarded-For` hop instead of the proxy's address.
