# Web UI

The browser app at `https://logbook.boogles.dev`. Code: `web/`. Design: the B3 "mono HUD" screens on the Doop canvas *Logbook web UI* and `web/DESIGN.md`. Hosting and origins: ADR 0011.

## Stack

- SvelteKit 3 with Svelte 5 runes, TypeScript (strict), Tailwind 4. Built with Bun.
- `@sveltejs/adapter-static` with an `index.html` fallback. Every route is client-rendered (`ssr = false` in the root layout). The build is plain files on Cloudflare Pages (ADR 0018), with no functions and nothing billable.
- One build-time setting: `LOGBOOK_API_URL`, the API origin (`https://api.logbook.boogles.dev`). It must be `https://`, or `http://localhost` / `127.0.0.1` for development.
- Fonts are self-hosted, so no third-party font requests:
  - Departure Mono (SIL OFL) for big figures and panel heads;
  - Geist for UI text;
  - Geist Mono for numbers and labels.

## Talking to the API

- Every call goes to `LOGBOOK_API_URL` with `credentials: 'include'`, so the browser sends the host-only `logbook_session` cookie of the API domain (same site, ADR 0011).
- `GET /api/v1/session` runs once on load and decides signed-in or signed-out. Its `csrf_token` is kept in memory only, never in storage, and sent as `X-CSRF-Token` on every `POST`, `PATCH` and `DELETE`.
- **Sign in** and **link Hackatime** are full-page navigations to `/auth/login?next=<path>` and `/auth/hackatime/link`, never `fetch`. Sign out is `DELETE /api/v1/session` with the CSRF header. `POST /auth/logout` can't be called cross-origin, because CORS covers `/api/` only.
- **Errors:**
  - A `401` from any `/api/v1/me` call means the session ended: the app clears its user and shows the signed-out home.
  - A `429` shows "too many requests, try again in N s" from `Retry-After`.
  - Any other error shows the server's `{"error"}` message in the panel that asked. Nothing fails silently.
- **Live:** `EventSource` on `/api/v1/me/live` (with credentials) while the tab is visible, closed when hidden. Each `minutes` event refetches what the page shows, at most once every 5 seconds.

## Pages

| Path | Who | Shows | API |
|---|---|---|---|
| `/` signed out | anyone | What Logbook does; "Sign in with Hack Club"; an example hour cut minute by minute (illustrative, labelled "example") | none |
| `/` signed in, no device yet, with history | user | Their editor history straight away (usually imported from Hackatime), all of it labelled "at the keyboard": the chosen range by day, languages and editors, projects, the year as a grid. On top, one strip with the single next step: the install command, and the Hackatime import's progress while it runs. Counted columns show "—" and say counting starts when the agent is installed. "What counts" rules sit beside the projects | `GET /api/v1/me/today`, `GET /api/v1/me/summary`, `GET /api/v1/me/projects`, `GET /api/v1/me/devices`, `GET /api/v1/hackatime/link`, `POST /api/v1/api_keys` |
| `/` signed in, no device yet, no history | user | Set-up steps: install command (creates an API key on request and shows it once), plugins and AI tools (automatic), Hackatime history (link and import progress); "What counts" rules | `POST /api/v1/api_keys`, `GET /api/v1/me/devices`, `GET /api/v1/hackatime/link` |
| `/` signed in | user | Today (counted, at the keyboard, the you / agent alone / idle split); streak and the last 14 days; this week, 30 days or year by day; languages and editors; projects; agent status; the year as a grid | `GET /api/v1/me/today`, `GET /api/v1/me/summary`, `GET /api/v1/me/projects`, `GET /api/v1/me/devices`, live |
| `/projects` | user | All projects: counted and keyboard time, last active, hidden or archived | `GET /api/v1/me/projects` |
| `/projects/[name]` | user | One project: totals, the last 30 days by day, languages; settings (GitHub repo, hidden, archived) | `GET /api/v1/me/projects/{name}`, `GET /api/v1/me/summary?project=`, `PATCH` |
| `/u/[username]` | public | Public profile: totals, top projects, languages; live while open | `GET /api/v1/users/{username}/profile`, public live |
| `/settings` | user | Profile (display name, time zone, public or private); API keys; devices; Hackatime link; apps with access; admin keys (admins only); sign out | session, profile, api_keys, devices, hackatime, grants, admin_keys |
| `/submissions` | user | Their submissions: project, dates, program, status, counted hours, evidence counts, decision; copy the review link; withdraw before a decision. On a project page, "Submit these hours" shows the one command to run (`logbook submit …`) with the dates filled in | `GET /api/v1/me/submissions`, `DELETE` |
| `/r/[token]` | anyone with the link | The review page (ADR 0020): headline counts against time at the keyboard and the one-third rule; the evidence line; what the hours are made of; cuts and flags with reasons; one strip per day with commits; AI sessions; the justification text with a copy button; the decision. Program members also get approve / adjust / reject. `noindex` | `GET /api/v1/review/{token}`, `POST …/decision` |
| `/apps`, `/apps/[id]` | user | Programs: register an app (client id, secret shown once, redirect URIs, public or not), rotate the secret, add reviewers; the program's submission inbox for members | `/api/v1/me/apps…` |
| `/oauth/authorize` | user | Consent: the app, its owner, what each scope lets it read; Allow / Deny | `GET`/`POST /api/v1/oauth/requests/{id}` |

**Leaving out what the API can't back.** The B3 mock had a "longest run", a programs panel and the dates of the best streak.
- The API has no endpoint for the first or the last, so those are dropped.

## Words

These match the API's two kinds of time (ADR 0014):

- **Counted** is credited time from the agent (`credited_*`).
- **At the keyboard** is raw time from editor heartbeats (`seconds`, `total_seconds`).
- **Agent alone** and **idle** are the agent's `agent` and `idle` minutes (`credited_labels`).

When `credited_*` is `null` (no device ever registered), the UI never shows counted zeros:

- **With history** (any time at the keyboard in the last year, for example from a Hackatime import), it shows that history, labelled "at the keyboard", with the install strip on top. A teen sees their own work in the first seconds, and nothing raw is ever called counted.
- **Without history**, it shows the set-up steps. While a Hackatime import is still fetching its first 30 days, there is nothing to show yet, so the set-up steps (with the import's progress) stay until the history arrives.
- The streak and Today panels need counted time, so they appear only once a device registers.

## Security (threat-model delta)

- **XSS:** Svelte escapes all text. No `{@html}` anywhere.
  - Project names, display names and repo URLs come from users and are rendered as text only.
  - Links to repos are built only from the `https://github.com/<owner>/<repo>` form the API enforces, and only rendered after the app checks they start with `https://github.com/`.
- **Headers:** `static/_headers` sends these on every response (Cloudflare Pages):
  - a strict CSP: `default-src 'self'`; `connect-src` the API origin; `img-src 'self' data:` (avatars are pixel identicons drawn from the username, so no remote images); `frame-ancestors 'none'`; `base-uri 'none'`; `form-action` the API origin;
  - `Referrer-Policy: strict-origin-when-cross-origin`, `X-Content-Type-Options: nosniff`, `Permissions-Policy` off for camera, microphone and geolocation.
- **API keys:** a new key is shown once, held only in component state, and gone on navigation. It is never written to storage or the URL. The install command shows the key masked until "Show" is pressed, and "Copy" copies the full command.
- **Redirects:** the `next` path sent to `/auth/login` is the current path only. The API validates it again.
- **Third parties:** no analytics, no third-party scripts or fonts.
