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
  - `303 See Other` to `{LOGBOOK_WEB_URL}/settings?hackatime=linked`.
  - `303 See Other` to `{LOGBOOK_WEB_URL}/settings?hackatime=denied` if the user declined on Hackatime.
  - The web UI is on its own origin (ADR 0011), so `cmd/logbook-server` passes the web origin's settings page as `HandlerOptions.SettingsPath`.
- **Errors:**
  - `400` invalid or expired state, missing or foreign cookie, or Hackatime rejected the code (expired, reused or PKCE mismatch). The UI should offer "start linking again".
  - `401` not signed in.
  - `403` the user's current link is to a Hackatime account that is banned (red), and they tried to link a *different* account. A banned link can't be swapped out, or the ban would go with it.
  - `409` this Hackatime account is already linked to another Logbook account. One Hackatime account can be linked to only one Logbook user.
  - `502` Hackatime unavailable or returned an invalid response.
  - `503` not configured.

A failed link never revokes the token Hackatime handed out. Hackatime reuses access tokens, so that token is usually the one already stored for an existing link, and revoking it would break that link.

Linking the same Hackatime account again (for example, to renew access) is allowed, even when it is banned. It replaces the token and extends the import to today. Days that were already fully imported aren't fetched again. The day of the previous link is fetched again, because it hadn't ended when it was imported. Linking a *different* Hackatime account restarts the import from the beginning and revokes the old account's token at Hackatime.

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
      "recent_from": "2026-10-22",
      "recent_done": true,
      "days_total": 1055,
      "days_done": 501,
      "heartbeats_imported": 183220,
      "last_error": "",
      "updated_at": "2026-11-20T09:40:11Z"
    }
  }
  ```

  - `hackatime_username`: Hackatime's OAuth profile doesn't expose a Hackatime username, so this is the GitHub username on the Hackatime account. It may be omitted when there isn't one.
  - `trust_level`: one of `blue` (unscored), `green` (trusted), `red` (banned) or `unknown`. As on Hackatime itself, "suspected" is shown to the user as `blue`. The raw level is used only for server-side gating. `unknown` after linking means Logbook can no longer verify the account, usually because the user revoked Logbook's access on Hackatime. The UI should offer "link again".
  - `import.status`: one of `pending`, `running`, `done` or `failed`.
    - The import fetches the last 30 days (`recent_from` to `to_date`) first, then backfills older history from `from_date`, oldest first. `recent_done` turns `true` once the last 30 days are in, which is usually within seconds of linking: the dashboard can show recent activity from then on, while the backfill carries on.
    - `next_date` is the backfill cursor: every day from `from_date` up to (not including) `next_date` has been imported. Once the backfill reaches `recent_from`, `next_date` moves on with the recent days.
    - Progress in percent is `days_done / days_total`. `days_done` counts the backfilled days and the recent days done. A `done` import always reports `days_done == days_total`.
    - `last_error` is a user-safe sentence, or `""`. When `failed`, the user can link again to resume. A `done` import may carry a note that some very busy days were capped (at most 50,000 heartbeats per day are imported).
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
  - `403` CSRF failure (from the middleware), or the linked Hackatime account is banned (red). A banned link can't be removed, so the ban keeps applying to this Logbook account. The user can still revoke Logbook's access in their Hackatime settings.
  - `404` no Hackatime account is linked.

## Background jobs (River, queue `hackatime`)

| Kind | When | What |
|---|---|---|
| `hackatime_import` | Queued on link (unique per user while active). | Imports in two phases. The recent phase fetches `recent_from` to `to_date` (the last 30 days), so the dashboard fills in first. The backfill phase then fetches `next_date` up to `recent_from`, oldest first. Each phase first asks `GET /api/v1/users/my/heartbeats/spans` where the user was active, one request per 31 days, and keeps probing through quiet months (up to 186 days ahead) until it has seen about 8 hours of activity. It then fetches `GET /api/v1/my/heartbeats` in windows of whole UTC days holding about 8 hours of activity each, so a quiet stretch shares one request with its neighbours. Every day is still fetched: spans only size the windows, because Hackatime leaves a lone heartbeat at the end of a range out of every span. If a probe fails (anything but a rate limit), that month is fetched one day per request instead. It splits windows whose response is too large (responses are capped at 8 MiB) and sends each response to ingest as soon as it arrives, in chunks of 1,000 with source `import`. At most 50,000 heartbeats are kept per UTC day. It fetches up to 2 windows at a time (oldest first) and saves the heartbeat count as each window finishes, but moves a phase's cursor only past windows that finished along with every window before them, so a crash resumes at the first unfinished window. It saves progress only up to the start of a day that hadn't ended when it was fetched, so the day of linking is fetched again on the next link. It pauses 500 ms after each heartbeats window. It yields every ~4 minutes and resumes. It snoozes on Hackatime rate limits. After 12 failed attempts, or if the token is revoked, it is marked `failed`. Every write is tied to the link the run started with, so a run overtaken by an unlink or relink stops without touching the new import. |
| `hackatime_import_sweep` | Every hour (periodic, leader only). | Queues `hackatime_import` for unfinished imports that haven't moved in 15 minutes (up to 500 per sweep). This recovers a relink that landed just as the previous job was finishing. |
| `hackatime_trust_sweep` | Every hour (periodic, leader only). | Queues `hackatime_trust_refresh` for links whose trust level is older than 24 hours (up to 500 per sweep). |
| `hackatime_trust_refresh` | Queued by the sweep. | Re-reads the trust level with the user's token. If Hackatime rejects the token (access revoked), a `red` level is kept and any other level becomes `unknown`. The public `trust_factor` endpoint is never used: it matches usernames as well as ids, so an alt account could answer for the user. Changes are audited (`hackatime.trust_changed`). |

All Hackatime calls from one server process (every import, the trust refresh and OAuth) share one token bucket: 2 requests a second sustained, with a burst of 2, or at most about 7,200 an hour. Hackatime allows 10,000 API requests per hour per IP, and every Logbook user shares the server's IP. Fetching windows in parallel never raises that rate. Requests get slots in the order they ask, and each import has at most 2 in flight, so imports running at once share the budget evenly. A 429 from Hackatime pauses every caller for its `Retry-After`, including callers already waiting for a slot: a pause wakes them to look again, so none goes out on a slot planned before it. A caller that would wait more than 30 seconds for a pause to end stops at once (also when the pause starts or grows while it waits), and its job snoozes until the pause ends. After a pause, requests resume at the sustained rate.

Relinking extends the import to the new day. If the previous recent days still reach into the new last 30 days, the recent phase carries on from where it stopped. Otherwise the new last 30 days become the recent phase, and the days in between are backfilled after them.
