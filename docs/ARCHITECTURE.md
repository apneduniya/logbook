# Architecture

See `docs/adr/` for the reasons behind each choice.

```
laptop                                     server (Go + Postgres)
┌──────────────────────────────┐          ┌───────────────────────────────┐
│ editor plugins ─┐            │          │ ingest (WakaTime-compatible)  │
│ AI harness hooks├─► agent ───┼─────────►│ accounts / profiles / projects│
│ OS idle + focus │   (Go)     │ minutes, │ live dashboard (SSE)          │
│ git watcher ────┘            │ hashes   │ submissions + review packets  │
│  SQLite evidence store       │          │ program API (Hackatime-shaped)│
│  pkg/struggle engine         │          │ admin API for Fraud Squad     │
└──────────┬───────────────────┘          └───────────────────────────────┘
           │ mirror heartbeats (user's own Hackatime key)
           ▼
       Hackatime
```

## Packages

| Path | Responsibility |
|---|---|
| `cmd/logbook-server` | Wires config, database, HTTP server and jobs. No logic |
| `cmd/logbook` | Agent daemon and CLI (`logbook login`, `status`, `submit`, ...) |
| `pkg/wire` | Wire types shared by server and agent (heartbeats, minute batches, evidence) |
| `pkg/struggle` | Pure rules engine: events in, labelled minutes and cuts out |
| `pkg/evidence` | Canonical encoding, hash chain, ed25519 signing and verification |
| `pkg/projectname` | Project name normalisation shared by heartbeat ingest and credited minutes |
| `internal/server/config` | Typed env config |
| `internal/server/store` | sqlc-generated queries and transaction helpers |
| `internal/server/auth` | HCA sign-in, sessions, API keys, CSRF |
| `internal/server/hackatime` | Hackatime client: OAuth, trust level, history import |
| `internal/server/ingest` | Heartbeat validation, dedup, rollups |
| `internal/server/stats` | Dashboard and program-API aggregates |
| `internal/server/devices` | Agent device keys: register, list, revoke |
| `internal/server/credited` | Credited-minute uploads from the agent |
| `internal/server/ledger` | Signed daily evidence chain per device |
| `internal/server/live` | SSE fan-out |
| `internal/server/httpapi` | HTTP handlers and middleware; thin, calls services |
| `internal/server/jobs` | River workers |
| `internal/agent/...` | Agent: proxy, sensors, harness plugins, git, local store, uploader, tray |
| `internal/e2e` | Tests only: the agent's crediting pipeline against the real minutes endpoint and Postgres (the one place importing both sides) |
| `web/` | SvelteKit UI |
| `migrations/` | goose SQL migrations |

## Build order

Each step leaves something usable.

1. **Server core:**
   - HCA sign-in and API keys;
   - WakaTime-compatible ingest;
   - Hackatime link and history import;
   - profiles, projects, live dashboard.

   After this step, Logbook already replaces Hackatime for normal coding.
2. **Agent:**
   - heartbeat proxy with Hackatime mirroring;
   - idle and focus sensors;
   - git watcher;
   - local store and uploader;
   - installer and tray for Windows, Mac and Linux.
3. **AI harness plugins:** Claude Code and Codex first, then Gemini CLI, OpenCode, Cursor and Copilot.
4. **Struggle engine and evidence:** minute labels, cut rules, signed hash chain.
5. **Submissions, review packets, the Hackatime-compatible program API, the admin API.**
6. **Hardening:**
   - low-spec Windows testing;
   - load test against the hosting budget;
   - privacy page;
   - docs.
7. **Pilot** with one program.
