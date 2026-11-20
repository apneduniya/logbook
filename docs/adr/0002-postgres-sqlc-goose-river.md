# 0002: Postgres with pgx, sqlc, goose and River

- Status: accepted
- Date: 2026-11-20

## Context

The server stores users, projects, heartbeats (with high write volume), per-minute rollups, submissions and audit logs. Hackatime runs comfortably on one Postgres instance at the same load. We also need background jobs: Hackatime import, rollups, GitHub sync and webhooks. Adding a separate queue service would mean more to run and more to pay for.

## Options considered

| Option | Pros | Cons |
|---|---|---|
| Postgres + pgx + sqlc + goose + River | Typed queries generated from plain SQL; migrations are plain SQL; jobs run in the same database inside transactions; nothing extra to run | Writing SQL by hand |
| Postgres + an ORM (GORM, ent) | Less SQL | Hidden queries, hard to tune the hot paths |
| Timescale / ClickHouse for heartbeats | Faster analytics | Another system to run; not needed at this scale |

## Decision

- PostgreSQL 17 as the system of record.
- `pgx/v5` as the driver and connection pool.
- `sqlc` generates typed Go from SQL in `internal/server/store/queries/`.
- `goose` for SQL migrations in `migrations/`.
- `River` for background jobs.
- Heartbeats are partitioned by month, deduplicated on `fields_hash` the same way Hackatime does, and rolled up into per-user, per-project, per-minute rows. Dashboards read only the rollups.

## Consequences

- One stateful service to back up.
- Every query is visible and reviewable in SQL files.
- Follow-up: load-test ingest and rollups at Hackatime's scale before launch (PRD goal 5).
