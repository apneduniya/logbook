# 0001: One Go monorepo for the server, agent and shared logic

- Status: accepted
- Date: 2026-11-20

## Context

Logbook has three parts:
- a central server;
- a background agent on Windows, Mac and Linux machines, often low-spec;
- a web UI.

The agent must be small, start fast and install without a runtime. The server has to fit a hosting budget of $500/month at 15.5k MAU, and the bounty is judged on maintainability by outside readers. The server and agent share data types (heartbeats, minute labels, evidence hashes).

## Options considered

| Option | Pros | Cons |
|---|---|---|
| Go for server and agent, one module | One language; static binaries for every OS; low RAM; shared types; simple cross-compiling | Fewer UI options for the agent's tray |
| Fork Hackatime (Rails) and add a Go agent | Feature-complete on day one | Two languages; a fork starts with no user ids or trust history; heavy for our scope (see research) |
| Rust agent, Go or TS server | Smallest agent | Two languages; slower to build |

## Decision

Go 1.27 for everything except the web UI. A single module, `github.com/apneduniya/logbook`:

```
cmd/logbook-server   server binary
cmd/logbook          agent + CLI binary
internal/server/...  server-only packages
internal/agent/...   agent-only packages
pkg/...              shared, stable types (wire formats, minute labels, evidence)
web/                 SvelteKit UI, built and embedded into the server
migrations/          SQL migrations
```

Dependencies point inward only: `cmd` → `internal/*` → `pkg`. `internal/server` and `internal/agent` never import each other.

## Consequences

- One CI pipeline, one test command, shared wire types and no drift between the two binaries.
- Hackatime is used only for inspiration and API compatibility. We don't copy its code: it's MIT, and copying MIT code into an AGPL project is allowed but would need attribution.
