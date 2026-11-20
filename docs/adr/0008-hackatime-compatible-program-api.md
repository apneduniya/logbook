# 0008: Programs read hours through a Hackatime-compatible API

- Status: accepted (OAuth provider library to be chosen in step 5)
- Date: 2026-11-20

## Context

25 of 27 program repos read Hackatime, mostly through OAuth `profile read`, `GET /api/v1/authenticated/me`, `GET /api/v1/authenticated/projects`, `GET /api/v1/users/:id/stats` and `trust_factor`. Making programs integrate with a new API would slow adoption.

## Decision

Logbook serves the same paths and response shapes for those calls, plus:
- `credited_seconds` and `review_url` fields per project. These are added fields, so existing clients ignore them.
- `GET /api/v1/submissions/:id`, which returns the review packet as JSON.

`total_seconds` returns credited time by default. Raw time is available via `?raw=true`.

## Consequences

- Programs that read Hackatime's base URL from config can switch by changing it. The rest need a small patch, which we'll write for them.
- Compatibility is pinned by contract tests built from the requests we found in program repos.
- Follow-up: choose an OAuth2 provider library (`ory/fosite`'s last release was Dec 2024, so check alternatives) in its own ADR.
