# Logbook: engineering rules

These rules apply to everyone who writes code here, human or agent. Read `docs/PRD.md`, `docs/ARCHITECTURE.md` and `docs/adr/` before changing behaviour.

## Principles

1. **Readable first.** Choose the obvious solution. A reviewer who has never seen the package should understand a function in one read.
2. **Small packages with one job.** Each package has a doc comment (`doc.go`) saying what it owns and what it doesn't. If you can't describe it in a sentence, split it.
3. **Dependencies point inward.** `cmd` → `internal/*` → `pkg`. `internal/server` and `internal/agent` never import each other. `pkg` never imports `internal`.
4. **Thin edges, pure core.** HTTP handlers, CLI commands and job workers parse input, call a service and write output. Business rules live in services and pure packages (`pkg/struggle`, `pkg/evidence`), which take plain values and do no I/O.
5. **Accept interfaces, return structs.** Define small interfaces in the package that uses them, not the one that implements them.
6. **No globals, no `init()` side effects.** Dependencies are passed in through constructors (`New(...)`). Configuration is read once, in `cmd/`.
7. **Errors carry context.** Wrap with `fmt.Errorf("doing X: %w", err)`. Use sentinel errors or typed errors for conditions callers handle. Never ignore an error; if dropping one is truly correct, write a comment saying why.
8. **Context everywhere.** Every function that does I/O takes `ctx context.Context` first and respects cancellation. Every outbound call has a timeout.
9. **Security by default.**
   - Validate all input at the edge.
   - Every data access is scoped to the authenticated user.
   - Secrets come from env only and are never logged.
   - Store tokens and keys as hashes, or encrypted when we need to read them back.
   - Follow `docs/THREAT-MODEL.md`.
10. **Privacy by default.** Only send off the device what the PRD's privacy rules allow. If unsure, keep it local.

## Conventions

- Go 1.27. Format with `gofmt`. `golangci-lint run` must be clean.
- **Logging:** `log/slog` with structured fields. No `fmt.Println` in non-CLI code.
- **SQL:**
  - Queries live in `internal/server/store/queries/*.sql` and are generated with `sqlc generate`. Don't hand-write SQL strings in Go.
  - Migrations are goose files in `migrations/`. Never edit a migration that has already been merged; add a new one.
- **HTTP:** stdlib `net/http` routing (`mux.HandleFunc("GET /path/{id}", ...)`).
  - JSON errors look like `{"error": "message"}`.
  - Compatibility endpoints match Hackatime and WakaTime exactly, and contract tests pin them.
- **Time:** store UTC. Unix seconds as `float64` only on WakaTime wire types; `time.Time` everywhere else.
- **Naming:** packages are short lowercase nouns. Don't use `util`, `common` or `helpers`.
- **Dependencies:** prefer the stdlib. A new dependency needs a reason in the PR, an exact pinned version and an active maintainer.

## Tests

- Table-driven unit tests for pure logic. Golden tests (`testdata/*.golden`) for `pkg/struggle` and wire formats.
- Store and HTTP tests run against real Postgres (`make db-up`), not mocks. Each test runs in a transaction that is rolled back.
- Test authorisation: every endpoint that reads user data needs a test proving another user can't read it.
- `make check` runs fmt, lint, vet, tests with `-race`, and `govulncheck`. It must pass before every commit.

## Commits

- Small commits, one concern each, written in the imperative ("Add heartbeat dedup").
- **No AI attribution lines of any kind** (no `Co-Authored-By`, no "Generated with" trailers). Commits are authored by the human maintainer only.
- Never commit secrets, `.env*` files or anything under `research/`.
