# Docs screenshots (web)

The web UI screenshots in `site/src/assets/shots/` come from a local demo stack with made-up data. The data is made through the real code paths: the agent's crediting pipeline and `logbook submit`, the server's API, re-check job and Postgres. Nothing is inserted by hand.

Needs the dev Postgres (`make db-up`, `127.0.0.1:54329`), Go, Bun, Node and Playwright's Chromium in `~/.cache/ms-playwright`.

```sh
# 1. Seed: drops and recreates the database logbook_demo (never "logbook").
#    Lays ten days of work over the ten UTC days ending today, so run it on
#    the day you take the screenshots, after 05:00 UTC.
go test -tags demoseed -run TestDemoSeed -count=1 -timeout 30m -v ./internal/e2e

# 2. Start the API (127.0.0.1:18480, dev sign-in) and the built web UI (localhost:18481).
scripts/docs-shots/stack.sh start

# 3. Capture. Pass names (e.g. web-review-rail) to take only those.
(cd scripts/docs-shots && bun install --frozen-lockfile)
node scripts/docs-shots/capture.mjs

# 4. The agent's local status page: runs the real agent in the foreground in a
#    temp dir (no autostart, no hooks), sends a few heartbeats, screenshots it,
#    then stops it, deletes the dir and revokes its key and device.
scripts/docs-shots/agent-status.sh

# 5. Stop the stack (logbook_demo stays).
scripts/docs-shots/stack.sh stop
```

What the seed makes (`internal/e2e/demoseed_test.go`, build tag `demoseed`, so the normal test run never sees it):

- **orpheus** ("Orpheus", public profile): a laptop registered through the devices service; ten days on `pixel-garden`, `tinyfs` and `hackclub-bot` with hands-on, AI-assisted and agent-alone minutes, a loop and a rubber-stamp streak (cut), idle gaps and commits; editor heartbeats sent by the agent's uploader. Hackatime is not linked: linking needs the real Hackatime OAuth.
- **reviewer-sam** owns the public app **Example Program**; **reviewer-alex** reviews for it.
- Three submissions made with `logbook submit` and re-checked by the server's job, all minutes verified: `tinyfs` (approved by Alex), `hackclub-bot` and `pixel-garden` (undecided).
- An OAuth grant (profile, read) from orpheus to Example Program, redeemed for a token.
- **nova**: signed up, no device (the set-up home and the consent page).

The capture script signs in with `POST /auth/dev-login`, which resets the display name to the username, so it sets the demo name back through `PATCH /api/v1/me/profile`. API key prefixes are cut to their first 4 characters; install keys are masked by the UI.

The encryption key is a fixed, public demo key shared by the seed and `stack.sh` (the server must use the seed's key to read stored review links). Use it for demo data only.
