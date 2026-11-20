# 0003: SvelteKit static UI embedded in the server binary

- Status: accepted; hosting superseded by 0011 (UI on Vercel, API on its own origin)
- Date: 2026-11-20

## Context

V1 needs public profiles, a personal dashboard that updates live, project pages, a submission flow and a reviewer view. It must feel designed, not generated, and it must not add a second server to run.

## Options considered

| Option | Pros | Cons |
|---|---|---|
| SvelteKit + adapter-static, embedded via `go:embed` | Rich UI; one binary to deploy; same family as Hackatime's Svelte UI, which Hack Clubbers already know | Profile pages are client-rendered (fixed with server-injected meta tags) |
| Go templates + htmx | No JS build step | A lower ceiling for interactive charts and the review timeline |
| Next.js | Our default for web products | Needs a second Node server and more hosting cost |

## Decision

- SvelteKit with Svelte 5, Tailwind 4 and adapter-static, built with Bun.
- The build output is embedded in `logbook-server` and served from there.
- The UI calls the server's JSON API.
- Live updates use Server-Sent Events.
- The server adds Open Graph meta tags to public profile pages.

## Consequences

- One binary to deploy, plus Postgres.
- The UI work follows the design-pass workflow.
- The JSON API is the only contract between the UI and the server.
