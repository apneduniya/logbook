# 0021: Our own OAuth provider, programs, and the Hackatime-shaped program API

- Status: accepted
- Date: 2026-11-25
- Settles: the open follow-up in ADR 0008 (the OAuth provider library)

## Context

ADR 0008 decided that programs read hours through the same calls they use with Hackatime: OAuth2 with scopes `profile read`, `GET /api/v1/authenticated/me`, `GET /api/v1/authenticated/projects` (the project picker), `GET /api/v1/users/:id/stats` and `trust_factor`. Sixteen of the 27 program repos surveyed use that OAuth flow; 25 read `users/:id/stats` (research, 19 Nov). None of it exists on our server yet.

Programs also need a place for their reviewers (ADR 0020 decisions) and a way to receive submissions.

Hackatime's provider is Doorkeeper. Its tokens last 16 years and are stored in plain text (Hackatime issues #1629, #1630). Programs never refresh tokens.

## Decision

### Library: none; a small provider in `internal/server/oauth`

We implement the authorization-code grant ourselves (RFC 6749 §4.1, PKCE per RFC 7636), plus client credentials (§4.4) for a program's own API calls. That's a few hundred lines on top of what `internal/server/auth` already does for sessions and API keys.
- `ory/fosite` had no release in over a year when checked, and is far larger than the two grants we need. `zitadel/oidc` is maintained but brings OpenID Connect, which no program uses.
- Every security property is pinned by tests: exact redirect URI match, single-use codes, PKCE, client authentication, scope checks, token hashing, and another-user and another-client access.

### Apps are programs

- Any signed-in user can register an app at `/settings/apps`, as on Hackatime. It has a name, a slug, one or more redirect URIs (exact match; `https://`, or `http://localhost` / `127.0.0.1` for development), and a homepage URL.
- The client secret is shown once, then stored as a SHA-256 hash. It can be rotated.
- **An app is also a program.** Its owner can add **reviewers** by Logbook username. Owner and reviewers are the program's members (ADR 0020 decisions). A user can't add themselves to someone else's program.
- An app can be marked `public` by its owner, so users can pick it when submitting (`--program <slug>`).

### Grants and tokens

- **Authorization code:** `GET /oauth/authorize` (on the API origin) checks the client, redirect URI, scope and PKCE parameters, then sends the browser to the web UI's consent page. The consent page shows the app, its owner and what each scope allows, and posts the approval (session + CSRF) to `POST /api/v1/oauth/authorize`, which returns the redirect with a code.
  - Codes are 256-bit random, stored hashed, single use, valid 10 minutes, bound to client, redirect URI, user, scope and PKCE challenge.
  - PKCE is required for public clients (no secret) and optional for confidential ones (Doorkeeper doesn't require it, and existing programs don't send it).
  - A user who already granted the same scopes to the same app is sent straight back, as Doorkeeper does.
- **Client credentials:** a program authenticates as itself to list and read submissions addressed to it.
- **Tokens:** opaque 256-bit, stored as SHA-256 hashes. Access tokens last one year; we also return a refresh token (programs that ignore it re-authorise once a year). `POST /oauth/revoke` (RFC 7009). Users see and revoke their grants in `/settings`.
- **Scopes:** `profile` (default; `authenticated/me`), `read` (projects, stats, heartbeats). Client credentials get `submissions`. Nothing else in V1.

### The program API (Hackatime-shaped)

Same paths, parameters and response shapes as Hackatime for what programs actually call (exact list and contract tests in `docs/api/programs.md`):
- `GET /api/v1/authenticated/me`, `GET /api/v1/authenticated/projects`
- `GET /api/v1/users/{id}/stats` with `start_date`, `end_date`, `features=projects`, `total_seconds`, `filter_by_project`, `filter_by_category`, `no_ai_coding`, `test_param`
- `GET /api/v1/users/{id}/trust_factor`, `GET /api/v1/users/{id}/heartbeats/spans`, `GET /api/v1/users/{id}/projects`, `…/project/{name}`, `…/projects/details`, `GET /api/v1/my/heartbeats`

**What `total_seconds` means:** counted (credited) time, as ADR 0008 says. `?raw=true` gives time at the keyboard instead. Every response also carries `credited_seconds` and `raw_seconds` side by side, so a program can show both. A user with no agent has `credited_seconds: 0` and `raw_seconds` from their heartbeats: a program asking for counted time gets zero rather than an unchecked number.

**Who `{id}` is:** Hackatime's ids are integers, and typed clients decode them as such, so every user gets a numeric `public_id` that Hackatime-shaped responses use as `id`. A path id is `my`/`current` (the token's user), then digits (`public_id`), then a username. Slack ids aren't supported in V1. Stats are public when the user's profile is public (Hackatime's rule for public stats); otherwise they need a `read` token from that user.

**Counted time is per project.** Credited minutes have no language or editor, so counted figures exist for totals and projects only. Language, editor and category breakdowns are always time at the keyboard, and the response says so.

**Trust:** `trust_factor` carries the user's Hackatime trust level when linked, masked as Hackatime masks it publicly (`yellow` reads as `blue`), and `blue` when not linked, so programs that gate on Hackatime bans keep working. Banned users can't submit (ADR 0020). Logbook doesn't run its own ban list in V1.

### Rate limits

Per token, or per IP for public reads: a burst of 600, refilled at 600 a minute. That's enough for a program backend syncing its users every few minutes. Stats responses for closed date ranges are cacheable (`Cache-Control: private, max-age=300`).

## Consequences

- A program that reads Hackatime's base URL from config switches by changing it, re-registering its OAuth app with us, and asking users to connect once. The 18 that hardcode the host need a small patch, which we'll write for the pilot program.
- We own the OAuth code and its tests. If a well-maintained library appears that fits, swapping it in doesn't change the API.
- Identity keyed on Hackatime ids (Telescreen, Lapse, the handbook's "Submitter Hackatime ID") still points at Hackatime. Users who linked Hackatime have their Hackatime id on `authenticated/me` (`hackatime_id`), so programs can join the two.
