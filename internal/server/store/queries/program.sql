-- Queries for the Hackatime-shaped program API (ADR 0021): user lookup by
-- public id or username, and range totals for packages stats and programapi.
-- Every aggregate is keyed by one user id that the caller has already
-- authorised.
--
-- Raw time is heartbeat time from user_minutes (label 'you'); counted time is
-- capped credited time from credited_project_minutes (ADR 0014). Hidden
-- projects are excluded by name through exclude_projects, which callers fill
-- for reads that aren't authorised by the user.
--
-- Heartbeats store a missing project as '', which the rollup and every
-- response call "Unknown" (package rollup). Queries on heartbeats compare
-- project names after that same mapping, so hiding "Unknown" hides those
-- heartbeats too.

-- name: ProgramUserByPublicID :one
SELECT id, public_id, username::text AS username, display_name, email, profile_public
FROM users WHERE public_id = $1;

-- name: ProgramUserByUsername :one
SELECT id, public_id, username::text AS username, display_name, email, profile_public
FROM users WHERE username = sqlc.arg(username)::citext;

-- name: ProgramUserByID :one
SELECT id, public_id, username::text AS username, display_name, email, profile_public
FROM users WHERE id = $1;

-- name: ProgramHackatimeLink :one
-- The parts of a Hackatime link the program API shows: never the token.
SELECT hackatime_user_id, hackatime_username, trust_level
FROM hackatime_links WHERE user_id = $1;

-- name: ProgramProjectSettings :many
-- The user's project settings rows that differ from the defaults.
SELECT name, repo_url, hidden, archived
FROM projects
WHERE user_id = $1 AND (hidden OR archived OR repo_url <> '');

-- name: ProgramRawTotals :many
-- Raw seconds per (project, language) in [from_time, to_time), with the
-- first and last minute seen. A NULL projects or categories array means no
-- filter; exclude_categories is compared case-insensitively.
SELECT m.project, m.language,
       sum(m.seconds)::bigint AS seconds,
       min(m.minute)::timestamptz AS first_minute,
       max(m.minute)::timestamptz AS last_minute
FROM user_minutes m
WHERE m.user_id = sqlc.arg(user_id)
  AND m.label = 'you'
  AND m.minute >= sqlc.arg(from_time)
  AND m.minute < sqlc.arg(to_time)
  AND (sqlc.narg(projects)::text[] IS NULL OR m.project = ANY (sqlc.narg(projects)::text[]))
  AND (sqlc.narg(categories)::text[] IS NULL OR m.category = ANY (sqlc.narg(categories)::text[]))
  AND NOT (lower(m.category) = ANY (sqlc.arg(exclude_categories)::text[]))
  AND NOT (m.project = ANY (sqlc.arg(exclude_projects)::text[]))
GROUP BY m.project, m.language;

-- name: ProgramRawUniqueCoding :one
-- Raw seconds of category 'coding' across the given projects, each minute
-- counted at most once (60 s), however many projects shared it.
SELECT coalesce(sum(t.seconds), 0)::bigint AS seconds
FROM (
    SELECT least(sum(m.seconds), 60) AS seconds
    FROM user_minutes m
    WHERE m.user_id = sqlc.arg(user_id)
      AND m.label = 'you'
      AND m.category = 'coding'
      AND m.minute >= sqlc.arg(from_time)
      AND m.minute < sqlc.arg(to_time)
      AND m.project = ANY (sqlc.arg(projects)::text[])
      AND NOT (m.project = ANY (sqlc.arg(exclude_projects)::text[]))
    GROUP BY m.minute
) t;

-- name: ProgramCreditedTotals :many
-- Counted (capped credited) seconds per project in [from_time, to_time). The
-- project filters apply after the 60-second cap (see the view).
SELECT v.project,
       coalesce(sum(v.seconds), 0)::bigint AS seconds,
       min(v.minute)::timestamptz AS first_minute,
       max(v.minute)::timestamptz AS last_minute
FROM credited_project_minutes v
WHERE v.user_id = sqlc.arg(user_id)
  AND v.minute >= sqlc.arg(from_time)
  AND v.minute < sqlc.arg(to_time)
  AND (sqlc.narg(projects)::text[] IS NULL OR v.project = ANY (sqlc.narg(projects)::text[]))
  AND NOT (v.project = ANY (sqlc.arg(exclude_projects)::text[]))
GROUP BY v.project;

-- name: ProgramHeartbeatCounts :many
-- Heartbeats per non-empty project in [from_time, to_time], with the first
-- and last heartbeat time.
SELECT h.project,
       count(*)::bigint AS heartbeats,
       min(h.time)::timestamptz AS first_time,
       max(h.time)::timestamptz AS last_time
FROM heartbeats h
WHERE h.user_id = sqlc.arg(user_id)
  AND h.time >= sqlc.arg(from_time)
  AND h.time <= sqlc.arg(to_time)
  AND h.project <> ''
  AND (sqlc.narg(projects)::text[] IS NULL OR h.project = ANY (sqlc.narg(projects)::text[]))
  AND NOT ((CASE WHEN h.project = '' THEN 'Unknown' ELSE h.project END) = ANY (sqlc.arg(exclude_projects)::text[]))
GROUP BY h.project;

-- name: ProgramHeartbeatRuns :many
-- Heartbeats in [from_time, to_time] split into runs wherever the gap to the
-- previous heartbeat is longer than gap_seconds, as Hackatime's to_span does.
-- Each run is its first and last heartbeat time in Unix seconds, in order.
WITH h AS (
    SELECT extract(epoch FROM hb.time)::float8 AS t, hb.id
    FROM heartbeats hb
    WHERE hb.user_id = sqlc.arg(user_id)
      AND hb.time >= sqlc.arg(from_time)
      AND hb.time <= sqlc.arg(to_time)
      AND (sqlc.narg(projects)::text[] IS NULL OR hb.project = ANY (sqlc.narg(projects)::text[]))
      AND NOT ((CASE WHEN hb.project = '' THEN 'Unknown' ELSE hb.project END) = ANY (sqlc.arg(exclude_projects)::text[]))
),
marked AS (
    SELECT h.t, h.id,
           CASE WHEN lag(h.t) OVER w IS NULL OR h.t - lag(h.t) OVER w > sqlc.arg(gap_seconds)::float8
                THEN 1 ELSE 0 END AS starts
    FROM h
    WINDOW w AS (ORDER BY h.t, h.id)
),
runs AS (
    SELECT marked.t, sum(marked.starts) OVER (ORDER BY marked.t, marked.id) AS run
    FROM marked
)
SELECT min(runs.t)::float8 AS first_time, max(runs.t)::float8 AS last_time
FROM runs
GROUP BY runs.run
ORDER BY runs.run;

-- name: ProgramHeartbeats :many
-- The user's heartbeats in [from_time, to_time], oldest first, at most
-- max_rows of them.
SELECT id, time, source, entity, type, category, project, branch, language, editor,
       machine, operating_system, user_agent, is_write, lines, lineno, cursorpos,
       line_additions, line_deletions, project_root_count, dependencies,
       ai_model, ai_session, ai_input_tokens, ai_output_tokens, ai_prompt_length,
       ai_line_changes, human_line_changes, fields_hash, created_at
FROM heartbeats
WHERE user_id = sqlc.arg(user_id)
  AND time >= sqlc.arg(from_time)
  AND time <= sqlc.arg(to_time)
  AND NOT ((CASE WHEN project = '' THEN 'Unknown' ELSE project END) = ANY (sqlc.arg(exclude_projects)::text[]))
ORDER BY time, id
LIMIT sqlc.arg(max_rows);
