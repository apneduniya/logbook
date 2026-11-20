# 0011: Web UI on Vercel; API on its own origin

- Status: accepted (supersedes the hosting part of ADR 0003)
- Date: 2026-11-20

## Context

Adarsh decided the web UI is hosted on Vercel (`logbook.boogles.dev`) and the API on our own server (`api.logbook.boogles.dev`): this VPS for dev, AWS or HQ hosting later. ADR 0003 had embedded the UI in the server binary.

There are two ways for the browser to reach the API:

| Option | Pros | Cons |
|---|---|---|
| Vercel rewrites `/api/*` and `/auth/*` to the API (one origin) | No CORS; cookies first-party; OAuth callback URLs on the web domain | Every request arrives from Vercel's IPs, so per-IP rate limits lump all users together. Vercel's egress IPs aren't published, so we can't safely trust its X-Forwarded-For. Long-lived SSE through a proxy is fragile. Extra latency |
| The browser calls `api.logbook.boogles.dev` directly (CORS with credentials) | Real client IPs (Caddy is the only proxy); SSE works directly; the agent and the browser use the same API | Needs a CORS allow-list and OAuth callback URLs on the API domain |

## Decision

**The browser calls the API directly.**

- **Origins.** The API's base URL is `https://api.logbook.boogles.dev`, and that's where the OAuth callbacks live (`/auth/callback`, `/auth/hackatime/callback`). The web URL is `https://logbook.boogles.dev`, set via `LOGBOOK_WEB_URL`.
- **CORS.** The API allows exactly the web origin, with credentials, for `/api/*`. No wildcard. Preflight results are cached. CSRF protection stays as it is (`X-CSRF-Token` header, which a cross-origin attacker can't set without passing CORS).
- **Cookies.** The session cookie stays host-only on the API domain, `SameSite=Lax`. `logbook.boogles.dev` and `api.logbook.boogles.dev` are the same site (`boogles.dev`), so browsers send it on credentialed fetches. It isn't a third-party cookie.
- **Redirects.** After sign-in, the API redirects to the web URL (or to a `next` path on it). Only that origin is allowed, so there are no open redirects.
- **UI.** The web UI is a static SvelteKit build (Svelte 5, Tailwind 4) deployed to Vercel. It uses no Vercel functions, so the Hobby plan has nothing billable to charge.

## Consequences

- Rate limits see real client IPs.
- The OAuth apps' redirect URIs must point at `api.logbook.boogles.dev`.
- Server work: a CORS middleware, `LOGBOOK_WEB_URL` config, and post-login redirects to the web origin. Each gets tests.
- The server no longer embeds the UI (ADR 0003's embedding is dropped). Public profile pages get their Open Graph tags from the static build, or later from a small edge function.
