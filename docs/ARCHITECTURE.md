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
| `cmd/logbook-server` | Builds config, database, services and jobs, and serves `router.New`. No logic |
| `cmd/logbook` | Agent daemon and CLI (`logbook login`, `status`, `submit`, ...) |
| `pkg/wire` | Wire types shared by server and agent (heartbeats, minute batches, evidence) |
| `pkg/struggle` | Pure rules engine: events in, labelled minutes and cuts out |
| `pkg/evidence` | Canonical encoding of the agent's logged rows, salted minute buckets under a Merkle root (resumable hasher), proofs that reveal some buckets and the completeness of minute ranges, hash chain, ed25519 signing and verification |
| `pkg/projectname` | Project name normalisation shared by heartbeat ingest and credited minutes |
| `pkg/replay` | Turns evidence rows and a computation key into engine input, and says what a replay compares; shared by the agent's live runs and replays and the server's submission re-check (ADR 0020) |
| `internal/server/config` | Typed env config |
| `internal/server/store` | sqlc-generated queries and transaction helpers |
| `internal/server/auth` | HCA sign-in, sessions, API keys, CSRF |
| `internal/server/hackatime` | Hackatime client: OAuth, trust level (and its history), history import |
| `internal/server/ingest` | Heartbeat validation, dedup, rollups |
| `internal/server/stats` | Dashboard aggregates and streaks (the program API reuses the streak) |
| `internal/server/programapi` | The Hackatime-shaped program read API (ADR 0021): `authenticated/*`, `users/{id}/*`, `my/heartbeats`; identifier resolution, trust masking, counted versus raw time. Credentials come through an injected `Authenticator` |
| `internal/server/devices` | Agent device keys: register, list, revoke |
| `internal/server/credited` | Credited-minute uploads from the agent |
| `internal/server/ledger` | Signed daily evidence chain per device: checks uploads (signature, chain and seq continuity) and reads stored records for the submission re-check |
| `internal/server/live` | SSE fan-out |
| `internal/server/submissions` | Submission upload, the re-check job, review packets, review links and decisions (ADR 0020) |
| `internal/server/oauth` | OAuth 2 provider: apps (programs) and their members (owner, reviewers), consent requests, codes, tokens, grants, and the bearer middleware (`RequireScope`, `OptionalScope`) the program API uses (ADR 0021) |
| `internal/server/programs` | The submissions addressed to a program, for its members (membership itself lives in `oauth`) |
| `internal/server/admin` | Read-only admin API for Fraud Squad, admin keys (ADR 0022) |
| `internal/server/httpapi` | HTTP handlers and middleware; thin, calls services |
| `internal/server/router` | The server's whole route set on one mux, the global middleware and the CORS skip list; adapts OAuth tokens and API keys to the program API's `Authenticator`. End-to-end tests of the program flow run against it |
| `internal/server/jobs` | River workers |
| `internal/agent/...` | Agent: proxy, sensors, harness plugins, git, local store (an append-only event log, ADR 0017), crediting, uploader, tray |
| `internal/e2e` | Tests only: the agent's crediting pipeline against the real minutes and evidence-day endpoints and Postgres, ending each test with the ADR 0017 checks (agent, server and replays at each minute's key agree; sealed blocks re-hash), plus the ADR 0019 checks (stored block fields and seq continuity; agent proofs verify against stored records, prove windows complete and replay to the stored rows). The one place importing both sides |
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
