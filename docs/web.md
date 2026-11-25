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
| `/projects/[name]` | user | One project: totals, the last 30 days by day, languages; "Submit these hours" ("Submissions" below); settings (GitHub repo, hidden, archived) | `GET /api/v1/me/projects/{name}`, `GET /api/v1/me/summary?project=`, `GET /api/v1/programs`, `PATCH` |
| `/u/[username]` | public | Public profile: totals, top projects, languages; live while open | `GET /api/v1/users/{username}/profile`, public live |
| `/settings` | user | Profile (display name, time zone, public or private); API keys; devices; Hackatime link; apps with access; admin keys (admins only); sign out | session, profile, api_keys, devices, hackatime, grants, admin_keys |
| `/submissions` | user | Their submissions ("Submissions" below): project, dates, program, status, counted hours, evidence counts, decision; filter All · Verifying · Ready · Decided; copy the review link; withdraw before a decision | `GET /api/v1/me/submissions`, `DELETE /api/v1/me/submissions/{id}` |
| `/r/[token]` | anyone with the link | The review page (ADR 0020, "Review page" below): headline counts against time at the keyboard and the one-third rule; the evidence line; what the hours are made of; cuts and flags with reasons; one strip per day with commits; AI sessions; what didn't verify; trust; the justification text with a copy button; the decision. Program members also get approve / adjust / reject. `noindex` | `GET /api/v1/review/{token}`, `POST …/decision`, `GET /api/v1/me/apps` (signed in) |
| `/apps` | user | The apps they own or review for, each with how many submissions wait for review; "Register an app". Linked from the account menu | `GET /api/v1/me/apps`, `GET …/{id}/submissions?status=ready&decision=none` per app |
| `/apps/new` | user | Register an app: name, slug, homepage, redirect URIs (1–5), takes submissions or not, confidential or public client | `POST /api/v1/me/apps` |
| `/apps/[id]` | members | The app as a record ("Apps" below): facts, credentials (client id, the secret shown once after register or rotate), the inbox, reviewers; owners also edit, rotate, add and remove reviewers, and delete | `GET`/`PATCH`/`DELETE /api/v1/me/apps/{id}`, `POST …/secret`, `…/members`, `…/submissions` |
| `/oauth/authorize` | user | Consent ("Consent" below): the app, its owner, what each scope lets it read, where you go back to; Allow / Deny. `noindex` | `GET`/`POST /api/v1/oauth/requests/{id}` |

**Leaving out what the API can't back.** The B3 mock had a "longest run", a programs panel and the dates of the best streak.
- The API has no endpoint for the first or the last, so those are dropped.

**Navigation.** The header's tabs are Today, Projects, Submissions, Profile and Settings: the work first (submissions are made from projects, so they sit next to them), then the person's own pages. Under 720 px five tabs don't fit one row, so Profile steps aside unless it is the open page; the account menu links it.

## Submissions

Submitting happens in the agent (`logbook submit`, [docs/agent/submit.md](agent/submit.md)), because only the computer that holds the signed log can build the evidence. The web UI shows the command and then the results.

- **"Submit these hours"** on `/projects/[name]` shows the one command with the project filled in, quoted for the browser's system (POSIX single quotes, or PowerShell's on Windows; a name starting with `-` goes after `--`). The days default to the 30 UTC days ending yesterday, and both can be changed. The page checks the range as the agent does (real days, last on or after first, ending yesterday UTC at the latest, at most 92 days) and says what's wrong once a day was changed or left; the command is still shown. A program from `GET /api/v1/programs` adds `--program <slug>`; if that list doesn't load, a sentence says how to add it by hand. A project with nothing counted says so instead of showing a command. `#lib/submit.ts` builds and checks it.
- **`/submissions`** is a run list (`SubmissionRow`): a status glyph, the project over its dates and program, counted hours with their evidence, the decision stamp or status word, then **Copy link** and **Withdraw**. A row with a review link opens its review page.
  - The filter (`Segmented`, with counts): All; Verifying; Ready (re-checked, no decision yet); Decided. Failed and withdrawn submissions show under All only.
  - Copy link is offered while the link works and there is something to review (not on failed or withdrawn submissions). The link is copied, never printed.
  - Withdraw asks once inline ("Withdraw it? The link stops working.") and is offered until there is a decision, failed submissions included, since withdrawing deletes the rows they uploaded. A `409` (decided meanwhile) shows the server's sentence by the row.
  - A failed row shows the server's `error` under it.
  - While any submission is verifying, the page asks again every 10 s and reloads when the set of verifying submissions changes.
  - Empty: `( NO SUBMISSIONS YET )`, the command with `PROJECT`, `YYYY-MM-DD` and `PROGRAM` placeholders (caps, not `<…>`, which a shell would read as redirects), and a link to the projects, whose pages fill it in.
- **Settings → Apps with access** (`#apps`, so the consent page can link to it): "You've given 2 apps access.", then one row per grant with the app's identicon (seeded by its slug), name, owner, what it reads ("profile and activity", from its scopes) and since when, and Revoke, which asks once. Revoking stops the app's tokens at once. With none, one sentence says when apps appear here.

## Review page

`/r/[token]` is public: the token is the credential, so it works signed out (with the public header) and signed in (with the app header). It reads like an audited timesheet, top to bottom: SUBMISSION (who, dates, program, version, rules, devices, how long raw logs are kept), HOURS (counted, at the keyboard and the one-third AI rule, in programs' one-decimal hours, then the evidence sentence with a `VERIFIED` tag or `2 DIFFER · 1,234 UNBACKED` linking to the list), MADE OF, CUTS AND FLAGS, DAYS, AI SESSIONS and NOT VERIFIED. On desktop a 4-column rail beside it holds TRUST, JUSTIFICATION and DECISION.

- **Times are UTC** everywhere on the page, and say so: the packet's days and strips are UTC days, and reviewers sit in many time zones.
- **States:**
  - `202` verifying: one panel with the meta and a `( VERIFYING )` spinner (the one use of the accent here, because it is live). The page asks again every 10 s and reloads when it's done.
  - `404` and `410` (withdrawn): a plain sentence each.
  - `failed`: a sentence for reviewers. The submitter, and only them, also sees the reason, read from `GET /api/v1/me/submissions/{id}`.
- **Who may decide.** The review endpoint doesn't say. Signed in, the page reads `GET /api/v1/me/apps` and treats the viewer as a member when one of their apps has the submission's program slug. The submitter is never a member here, even if they are one, because the server refuses their decision. The server checks membership again on every decision, so this only decides which controls to show.
- **Deciding.** Each control asks once, inline:
  - Approve takes no reason, because the API refuses one.
  - Adjust takes hours (decimal, below the counted time) and a reason.
  - Reject takes a reason.

  Reasons are 1–500 characters; the fields are checked on submit. Every decision sends the `version` the page read. A `409` for a newer version says "checked again since you read it" and offers Reload. Other `403`/`409` answers (not a member, banned or switched Hackatime account) show the server's sentence by the control. A `401` doesn't sign the app out or leave the page: the reason stays and a "Sign in again" button appears (`Api.decide`, and the membership read, pass `keepSession`). After a decision, members can record a new one; the newest counts.
- **Phones:** one column. For members the decision moves up under the hours.
- **The token is never printed.** It isn't in the title or any link the page renders. The justification is copied in full but shown with its link masked (`/r/AbCd••••`). The page sets `<meta name="robots" content="noindex">` and `<meta name="referrer" content="no-referrer">`, and `static/_headers` sends `X-Robots-Tag: noindex` and `Referrer-Policy: no-referrer` for `/r/*`. Commit links go to GitHub only, checked again in the browser.
- **Sign-in from a review link** passes `/r/<token>` as `next` to `/auth/login`. That is first-party (the API stores it with the login state and logs paths without query strings), and it's the only way back to the link after signing in.

## Apps

An app is also a program (docs/api/programs.md). `/apps/[id]` is a record page, after Linear's app page: what the app is, apart from how it authenticates.

- **Layout:** the title line (the slug's identicon, the name, who made it and your role), then FACTS (slug, homepage, redirect URIs, whether it takes submissions, client type) beside CREDENTIALS on the 8/4 grid, the INBOX across the page, and REVIEWERS with the owner's DELETE APP under it. On phones it is one column; reviewers get the inbox first, because it is what they come for.
- **Owner-only actions are hidden** for reviewers, not disabled: Edit, Rotate secret, adding and removing reviewers, Delete app. The API refuses them anyway (`403`).
- **The form** (register and edit) uses the Settings `.row` layout. The slug follows the name until it is typed by hand, and its hint shows the `logbook submit … --program <slug>` it makes. Redirect URIs are 1–5 fields with "Add another"; blank fields are dropped. Fields are checked on submit and once left, with the API's rules (`#lib/apps.ts`, unit-tested): `https://`, or `http://` only on `localhost` and `127.0.0.1`, no user info, fragment or spaces. The server's answers get plain sentences (a taken slug goes under the slug field). Slug and client type can't change after register, so the edit form leaves them out.
- **The client secret** is shown once, masked until "Show" (`CopyField`), right after register or rotate, with "Copy it now. Logbook keeps only a hash, so it can't be shown again." The register page hands it to the app's page in memory (`#lib/secret.ts`): the first read takes it, so a reload or a later visit shows only "Set <date> UTC". Rotate, Remove and Delete ask once inline (`ConfirmButton`).
- **The inbox** lists `SubmissionRow`s with the submitter column. A `Segmented` filter picks To review (ready with no decision yet; the default), Verifying, Approved, Adjusted, Rejected or All, 100 at a time with "Show 100 more". Each row opens its review page; "Copy link" copies the review link for the program's tracker, which is never printed. It loads by itself, so a filter change doesn't reload the page, and it has loading, empty (per filter) and error states.
- **Navigation:** the header's account menu has "Apps". It isn't a tab, because most people never make one.
- Loads depend on `logbook:apps` (`APPS` in `#lib/apps.ts`), not `ME`, so live minute events don't reset a form being typed.

## Consent

`/oauth/authorize` is where the API's `GET /oauth/authorize` sends the browser. The page has only the wordmark in its header (`PublicHeader bare`), signed in or not, so the choice is the only thing on it.

- **The sheet** (about 520 px, centred): the app's identicon → yours, "**Stardance** by fiona-hackworth wants to:", one row per scope with the server's own description, "Either way, you go back to `stardance.example`" (both answers redirect there), then **Allow** (solid ink, the second primary) and Deny (secondary, the same height). Under it: who you're signed in as, "Logbook doesn't review apps, so only allow apps you trust", and where to remove access (Settings → Apps with access, `/settings#apps`). The panel's meta is when the request expires, in your time zone.
- **States:**
  - `?error=invalid_client` or `invalid_redirect_uri`: "This app's link is broken. Tell the program." and why, with no buttons and no redirect.
  - No `?request=`: a sentence, no buttons.
  - Signed out: "Sign in with Hack Club", which comes straight back to this address (the request id travels in `next`, first-party).
  - `already_granted`: approved without asking; the page says so while it leaves.
  - `404` (unknown, expired, answered, someone else's), on load or on answering: "This request has ended", with no buttons.
- **Leaving:** Allow and Deny post `{approve}`, then set `window.location` to the returned `redirect`, but only after checking it is `http(s)` with the request's own redirect URI's origin and path (`safeRedirect`). Anything else shows a sentence and goes nowhere.

## Shared components

In `web/src/lib/components`, each with a header comment listing its props:

- `MinuteStrip`: a stretch of minutes in the packet's alphabet. It has commit ticks with a hover, focus or tap detail and a hidden table of runs. It also draws the signed-out home's example hour.
- `CopyField`: a value and a Copy button, with an optional masked value and Show (one-time secrets), a fixed `display` (review links) and prose wrapping.
- `DecisionStamp`: the inverted-ink verdict.
- `ReasonConfirm`: ConfirmButton with a reason and optional hours.
- `SubmissionRow` and `SubmissionListHead`: the run-list row for `/submissions` and an app's inbox.

The pure logic behind them is unit-tested: `#lib/strip.ts` (runs), `#lib/review.ts` (packet to view model, who is viewing, decision errors), `#lib/reason.ts` (field checks) and `#lib/submissions.ts` (row words).

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
- **Review tokens and secrets:** a review link is copied, never printed in full (lists copy it; the review page masks it inside the justification). A client secret is shown only once, masked until "Show", right after create or rotate. It goes from the register page to the app's page in memory only (`#lib/secret.ts`), never in storage, history state or the URL.
- **Consent:** the browser is sent only to an `http(s)` redirect with the request's own redirect URI's origin and path. `static/_headers` sends `X-Robots-Tag: noindex` for `/oauth/*`.
- **Third parties:** no analytics, no third-party scripts or fonts.
