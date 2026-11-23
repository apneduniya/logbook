# Logbook web

The browser UI for Logbook: a static SvelteKit app (Svelte 5, Tailwind 4) deployed to Vercel. It talks to the Logbook API on its own origin.

- **Behaviour, pages and security:** [`docs/web.md`](../docs/web.md)
- **Look and feel:** [`DESIGN.md`](DESIGN.md)

## Develop

The UI needs a running API on `localhost`, because the session cookie is same-site with the dev server only when both use `localhost`:

```sh
# terminal 1, repo root: an API with dev sign-in
LOGBOOK_ADDR=127.0.0.1:8080 LOGBOOK_BASE_URL=http://localhost:8080 \
LOGBOOK_WEB_URL=http://localhost:5173 LOGBOOK_DEV_LOGIN=true LOGBOOK_DEV_INSECURE_COOKIE=true \
LOGBOOK_ENCRYPTION_KEY=$(head -c32 /dev/urandom | base64) make run

# terminal 2
cd web && bun install
LOGBOOK_API_URL=http://localhost:8080 bun run dev
```

Sign in during development by posting the dev-login form, from a page on `http://localhost:5173` (see `docs/api/auth.md`, `POST /auth/dev-login`).

## Checks

```sh
bun run check     # svelte-check (types)
bun run lint      # prettier + eslint
bun run test      # vitest unit tests
LOGBOOK_API_URL=https://api.logbook.boogles.dev bun run build
```

`LOGBOOK_API_URL` is required for builds (dev, checks and tests default to `http://localhost:8080`). It is inlined into the bundle and the Content-Security-Policy.

## Deploy

On Vercel, set the project's root directory to `web` and set `LOGBOOK_API_URL`. `vercel.json` holds the build command, the SPA rewrite to `200.html` and the security headers.
