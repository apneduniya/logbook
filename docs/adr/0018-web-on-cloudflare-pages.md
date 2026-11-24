# 0018: Web UI on Cloudflare Pages

- Status: accepted (supersedes the hosting choice in ADR 0011)
- Date: 2026-11-23

## Context

ADR 0011 put the static web UI on Vercel. On its first day there, Vercel's bot checkpoint began answering requests from our own server with a JavaScript challenge, including headless browsers used for checks and Vercel's own API. The Hobby plan is also limited to non-commercial use and 100 GB of transfer a month.

The UI is plain static files (no functions), so any static host works. The API cannot move: it is a Go server with Postgres and long-lived SSE, which on Cloudflare would need paid Containers and an external database.

| Option | Pros | Cons |
|---|---|---|
| Stay on Vercel | Deploys on every push to `main` with no setup | Bot challenges we don't control; non-commercial clause; transfer cap |
| Cloudflare Pages | Free, no transfer cap, commercial use allowed; no challenge on static sites unless enabled; the same account already hosts our other `boogles.dev` sites | No GitHub connection yet; deploys run `make web-deploy` from our server (see Consequences) |

## Decision

**The web UI is the Cloudflare Pages project `logbook`, at `logbook.boogles.dev`.**

- The build's fallback page is `index.html`. Pages serves it for any path that isn't a file, because the build has no `404.html` (Pages' SPA mode). Real files such as fonts are served as themselves.
- `web/static/_headers` carries the security headers that can't be a `<meta>` tag (`frame-ancestors`, HSTS and the rest). The CSP stays a `<meta>` tag written by SvelteKit at build time.
- `make web-deploy` runs `make web-check`, then uploads `web/build` with wrangler.
- Everything in ADR 0011 about origins, CORS, cookies and redirects is unchanged.

## Consequences

- Deploys are automatic from the maintainer's server: a timer checks `main` every 3 minutes and runs `make web-deploy` when `web/` or the Makefile changed, retrying on failure. The Cloudflare token stays on that server (it only works from its IPs), so it is never a GitHub secret. Connecting the repo in the Cloudflare dashboard would replace this.
- `vercel.json` is gone. The Vercel project no longer serves the domain.
