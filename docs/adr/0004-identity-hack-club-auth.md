# 0004: Sign in with Hack Club Auth; carry over Hackatime identity and trust

- Status: accepted
- Date: 2026-11-20

## Context

Programs, Fraud Squad tools, bans and Airtable records are all keyed to Hackatime user ids and trust levels. A tracker that starts with new identities and no trust history won't be trusted. Hackatime itself signs users in through Hack Club Auth (HCA, `auth.hackclub.com`, `GET /api/v1/me`).

## Decision

- **Sign-in:** OAuth/OIDC against HCA. The Logbook account is keyed to the HCA identity id.
- **Linking Hackatime:**
  - The user links their Hackatime account with Hackatime OAuth (`profile read`). We store the Hackatime user id and read the trust level.
  - Users with Hackatime trust level "red" (banned) can't submit hours.
- **History import:** with the same OAuth token, a River job pages through `GET /api/v1/my/heartbeats`, one day at a time.
- **Tokens:** stored encrypted at rest with AES-GCM, using a key from env. Only the minimum scopes are requested.
- **Sessions:** server-side sessions in Postgres. The cookie is `HttpOnly`, `Secure` and `SameSite=Lax`, and holds a random id that is hashed in the database.

## Consequences

- Programs and Fraud Squad can match Logbook users to Hackatime records.
- Ban evasion is blocked as long as bans live on Hackatime.
- Follow-up: register OAuth apps with HCA and Hackatime (Ketan / HQ).
