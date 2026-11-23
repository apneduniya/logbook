# 0018: Web UI on Cloudflare Pages

- Status: accepted (supersedes the hosting choice in ADR 0011)
- Date: 2026-11-23

## Context

ADR 0011 put the static web UI on Vercel. On its first day there, Vercel's bot checkpoint began answering requests from our own server with a JavaScript challenge, including headless browsers used for checks and Vercel's own API. The Hobby plan is also limited to non-commercial use and 100 GB of transfer a month.

The UI is plain static files (no functions), so any static host works. The API cannot move: it is a Go server with Postgres and long-lived SSE, which on Cloudflare would need paid Containers and an external database.

| Option | Pros | Cons |
|---|---|---|
| Stay on Vercel | Deploys on every push to `main` with no setup | Bot challenges we don't control; non-commercial clause; transfer cap |
| Cloudflare Pages | Free, no transfer cap, commercial use allowed; no challenge on static sites unless enabled; the same account already hosts our other `boogles.dev` sites | Automatic deploys from GitHub need a one-time dashboard connection or a Pages-only token in CI; until then we deploy with `make web-deploy` |

## Decision

**The web UI is the Cloudflare Pages project `logbook`, at `logbook.boogles.dev`.**

- The build's fallback page is `index.html`. Pages serves it for any path that isn't a file, because the build has no `404.html` (Pages' SPA mode). Real files such as fonts are served as themselves.
- `web/static/_headers` carries the security headers that can't be a `<meta>` tag (`frame-ancestors`, HSTS and the rest). The CSP stays a `<meta>` tag written by SvelteKit at build time.
- `make web-deploy` runs `make web-check`, then uploads `web/build` with wrangler.
- Everything in ADR 0011 about origins, CORS, cookies and redirects is unchanged.

## Consequences

- No automatic deploys yet. Connecting the repo in the Cloudflare dashboard (or adding a Pages-only token as a GitHub secret) would restore them.
- `vercel.json` is gone. The Vercel project no longer serves the domain.
