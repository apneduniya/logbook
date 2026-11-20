# Hackatime link API

Links a Logbook account to the user's Hackatime account (ADR 0004), imports their Hackatime history in the background, and keeps their Hackatime trust level current.

All four endpoints need a browser session (`RequireSession`). `DELETE` also needs the `X-CSRF-Token` header, which the session middleware checks. Errors are JSON: `{"error": "message"}`.

## `GET /auth/hackatime/link`

Starts linking. The web UI points a normal link or button at this URL. Don't call it with `fetch`.

- **Auth:** session.
- **Response:** `302 Found` to `{HACKATIME}/oauth/authorize` with `client_id`, `redirect_uri` (`{LOGBOOK_BASE_URL}/auth/hackatime/callback`), `response_type=code`, `scope=profile read`, `state` and an S256 `code_challenge`. It also sets the `logbook_hackatime_link` cookie, which holds the sealed state and PKCE verifier. The cookie is `HttpOnly`, `Secure`, `SameSite=Lax`, uses `Path=/auth/hackatime` and lasts 10 minutes.
- **Errors:**
  - `401` not signed in.
  - `503` Hackatime OAuth isn't configured on this server (`LOGBOOK_HACKATIME_CLIENT_ID` / `_CLIENT_SECRET`).

## `GET /auth/hackatime/callback`

Hackatime redirects the browser here after the user consents. The web UI doesn't call this endpoint directly.

- **Auth:** session. It must be the same user who started the link. The state cookie is bound to that user.
- **Query:** `code`, `state`, or `error` if the user declined.
- **What it does:**
  1. Checks `state` against the cookie and the cookie's expiry.
  2. Exchanges the code with the PKCE verifier.
  3. Reads `GET /api/v1/authenticated/me` (Hackatime id, trust level, GitHub username).
  4. Stores the access token AES-256-GCM encrypted, with the user id as associated data.
  5. Writes an audit log entry (`hackatime.linked`) and queues the history import.
  6. Clears the state cookie in every case.
- **Response:**
  - `303 See Other` to `/settings?hackatime=linked`.
  - `303 See Other` to `/settings?hackatime=denied` if the user declined on Hackatime.
- **Errors:**
  - `400` invalid or expired state, missing or foreign cookie, or Hackatime rejected the code (expired, reused or PKCE mismatch). The UI should offer "start linking again".
  - `401` not signed in.
  - `409` this Hackatime account is already linked to another Logbook account. One Hackatime account can be linked to only one Logbook user. The token issued for the rejected attempt is revoked.
  - `502` Hackatime unavailable or returned an invalid response.
  - `503` not configured.

Linking the same Hackatime account again (for example, to renew access) is allowed. It replaces the token and extends the import to today. Days that were already imported aren't fetched again. Linking a *different* Hackatime account restarts the import from the beginning.

## `GET /api/v1/hackatime/link`

Link status and import progress for the signed-in user only.

- **Auth:** session.
- **Response `200`** when not linked:

  ```json
  {"linked": false}
  ```

- **Response `200`** when linked:

  ```json
  {
    "linked": true,
    "hackatime_user_id": 4242,
    "hackatime_username": "octocat",
    "trust_level": "green",
    "trust_checked_at": "2026-11-20T09:12:03Z",
    "linked_at": "2026-11-20T09:12:03Z",
    "import": {
      "status": "running",
      "from_date": "2024-01-01",
      "to_date": "2026-11-20",
      "next_date": "2025-04-17",
      "days_total": 1055,
      "days_done": 472,
      "heartbeats_imported": 183220,
      "last_error": "",
      "updated_at": "2026-11-20T09:40:11Z"
    }
  }
  ```

  - `hackatime_username`: Hackatime's OAuth profile doesn't expose a Hackatime username, so this is the GitHub username on the Hackatime account. It may be omitted when there isn't one.
  - `trust_level`: one of `blue` (unscored), `green` (trusted), `red` (banned) or `unknown`. As on Hackatime itself, "suspected" is shown to the user as `blue`. The raw level is used only for server-side gating.
  - `import.status`: one of `pending`, `running`, `done` or `failed`.
    - Progress in percent is `days_done / days_total`.
    - `last_error` is a user-safe sentence, or `""`. When `failed`, the user can link again to resume.
  - `import`: omitted if no import exists.
- **Errors:** `401` not signed in.
- No token or ciphertext is ever returned.

## `DELETE /api/v1/hackatime/link`

Unlinks the account.

- **Auth:** session + `X-CSRF-Token`.
- **What it does:**
  1. Deletes the stored token and the import state.
  2. Writes an audit entry (`hackatime.unlinked`).
  3. Revokes the token at Hackatime (`POST /oauth/revoke`, best effort).
  4. Leaves heartbeats that were already imported in place.
- **Response:** `204 No Content`.
- **Errors:**
  - `401` not signed in.
  - `403` CSRF failure (from the middleware).
  - `404` no Hackatime account is linked.

## Background jobs (River, queue `hackatime`)

| Kind | When | What |
|---|---|---|
| `hackatime_import` | Queued on link (unique per user while active). | Fetches `GET /api/v1/my/heartbeats` one UTC day at a time from `next_date` to `to_date`. It splits busy days into smaller windows, sends heartbeats to ingest in chunks of 1,000 with source `import`, and saves progress after each day. It yields every ~4 minutes and resumes. It snoozes on Hackatime rate limits. After 12 failed attempts, or if the token is revoked, it is marked `failed`. |
| `hackatime_trust_sweep` | Every hour (periodic, leader only). | Queues `hackatime_trust_refresh` for links whose trust level is older than 24 hours (up to 500 per sweep). |
| `hackatime_trust_refresh` | Queued by the sweep. | Re-reads the trust level with the user's token, falling back to the public `GET /api/v1/users/:id/trust_factor` if the token was revoked. Changes are audited (`hackatime.trust_changed`). |

All Hackatime calls from one server process are spaced at least 500 ms apart. Hackatime allows 10,000 API requests per hour per IP, and every Logbook user shares the server's IP.
