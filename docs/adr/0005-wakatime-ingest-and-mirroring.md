# 0005: WakaTime-compatible ingest, with mirroring to Hackatime from the agent

- Status: accepted
- Date: 2026-11-20

## Context

Every editor plugin Hack Clubbers use speaks the WakaTime heartbeat protocol through `wakatime-cli`. During the transition, programs must keep reading hours from Hackatime.

## Decision

- **Server:** accepts `POST /api/v1/users/current/heartbeats(.bulk)` in WakaTime's format, authenticated with a per-user Logbook API key. Heartbeats are deduplicated on `fields_hash`.
- **Agent:**
  - It runs a localhost endpoint, and the installer points `api_url` in `~/.wakatime.cfg` at it, so editor plugins keep working unchanged.
  - It sends each heartbeat to Logbook and, when the user has enabled mirroring (on by default), forwards the same heartbeat to Hackatime using the user's existing Hackatime key from the original config.
  - **The Hackatime key never leaves the device.**
- **Offline:** if the agent is offline, `wakatime-cli` queues heartbeats itself; if the server is unreachable, the agent queues them in its local database.

## Consequences

- Programs still see Hackatime hours, and Logbook is the source of record.
- Once programs read from Logbook, mirroring can be turned off per user or per program.
- The server never stores Hackatime write credentials.
