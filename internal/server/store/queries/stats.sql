-- Queries for the stats, projects and profiles packages.
-- Aggregates read user_minutes only, never heartbeats (ADR 0002).
-- Credited time is label = 'you'; other labels are reported separately.

-- name: StatsUserTimezone :one
SELECT timezone FROM users WHERE id = $1;

-- name: StatsBreakdown :many
-- One pass over the range, returned as (dimension, key, seconds) rows.
-- Days are local dates in the given IANA time zone.
WITH m AS (
    SELECT project, language, editor, category, label,
           (minute AT TIME ZONE sqlc.arg(tz)::text)::date AS day,
           sum(seconds)::bigint AS seconds
    FROM user_minutes
    WHERE user_id = sqlc.arg(user_id)
      AND minute >= sqlc.arg(from_time)
      AND minute < sqlc.arg(to_time)
      AND (sqlc.narg(project)::text IS NULL OR project = sqlc.narg(project)::text)
    GROUP BY 1, 2, 3, 4, 5, 6
)
SELECT 'label'::text AS dimension, m.label::text AS key, sum(m.seconds)::bigint AS seconds
FROM m GROUP BY m.label
UNION ALL
SELECT 'project'::text, m.project::text, sum(m.seconds)::bigint
FROM m WHERE m.label = 'you' GROUP BY m.project
UNION ALL
SELECT 'language'::text, m.language::text, sum(m.seconds)::bigint
FROM m WHERE m.label = 'you' GROUP BY m.language
UNION ALL
SELECT 'editor'::text, m.editor::text, sum(m.seconds)::bigint
FROM m WHERE m.label = 'you' GROUP BY m.editor
UNION ALL
SELECT 'category'::text, m.category::text, sum(m.seconds)::bigint
FROM m WHERE m.label = 'you' GROUP BY m.category
UNION ALL
SELECT 'day'::text, to_char(m.day, 'YYYY-MM-DD'), sum(m.seconds)::bigint
FROM m WHERE m.label = 'you' GROUP BY m.day;

-- name: StatsQualifyingDays :many
-- Local dates on which the user was credited at least min_seconds.
SELECT to_char((minute AT TIME ZONE sqlc.arg(tz)::text)::date, 'YYYY-MM-DD')::text AS day
FROM user_minutes
WHERE user_id = sqlc.arg(user_id) AND label = 'you'
GROUP BY (minute AT TIME ZONE sqlc.arg(tz)::text)::date
HAVING sum(seconds) >= sqlc.arg(min_seconds)::bigint
ORDER BY (minute AT TIME ZONE sqlc.arg(tz)::text)::date;

-- name: StatsQualifyingDaysBetween :many
-- Like StatsQualifyingDays, but only for minutes in [from_time, to_time).
-- Callers pass whole local days so each day's sum is complete.
SELECT to_char((minute AT TIME ZONE sqlc.arg(tz)::text)::date, 'YYYY-MM-DD')::text AS day
FROM user_minutes
WHERE user_id = sqlc.arg(user_id) AND label = 'you'
  AND minute >= sqlc.arg(from_time)
  AND minute < sqlc.arg(to_time)
GROUP BY (minute AT TIME ZONE sqlc.arg(tz)::text)::date
HAVING sum(seconds) >= sqlc.arg(min_seconds)::bigint
ORDER BY (minute AT TIME ZONE sqlc.arg(tz)::text)::date;

-- name: ProjectsMinuteTotals :many
-- Per (project, language): credited seconds and last activity of any label.
SELECT project, language,
       coalesce(sum(seconds) FILTER (WHERE label = 'you'), 0)::bigint AS seconds,
       max(minute)::timestamptz AS last_minute
FROM user_minutes
WHERE user_id = sqlc.arg(user_id)
  AND (sqlc.narg(project)::text IS NULL OR project = sqlc.narg(project)::text)
GROUP BY project, language;

-- name: ProjectsSettingsList :many
SELECT * FROM projects WHERE user_id = $1;

-- name: ProjectsSettingsGet :one
SELECT * FROM projects WHERE user_id = $1 AND name = $2;

-- name: ProjectsSettingsEnsure :exec
-- Creates the settings row with defaults if it doesn't exist yet, so that
-- ProjectsSettingsGetForUpdate always has a row to lock.
INSERT INTO projects (user_id, name) VALUES ($1, $2)
ON CONFLICT (user_id, name) DO NOTHING;

-- name: ProjectsSettingsGetForUpdate :one
-- Locks the row until the transaction ends, so concurrent updates are
-- serialised and each audit entry sees the true previous values.
SELECT * FROM projects WHERE user_id = $1 AND name = $2 FOR UPDATE;

-- name: ProjectsSettingsUpsert :one
-- Partial update: a NULL argument keeps the current (or default) value.
INSERT INTO projects (user_id, name, repo_url, hidden, archived)
VALUES (
    sqlc.arg(user_id), sqlc.arg(name),
    coalesce(sqlc.narg(repo_url)::text, ''),
    coalesce(sqlc.narg(hidden)::boolean, false),
    coalesce(sqlc.narg(archived)::boolean, false)
)
ON CONFLICT (user_id, name) DO UPDATE SET
    repo_url   = coalesce(sqlc.narg(repo_url)::text, projects.repo_url),
    hidden     = coalesce(sqlc.narg(hidden)::boolean, projects.hidden),
    archived   = coalesce(sqlc.narg(archived)::boolean, projects.archived),
    updated_at = now()
RETURNING *;

-- name: ProfileGetByID :one
SELECT id, username::text AS username, display_name, avatar_url, timezone, profile_public, created_at
FROM users WHERE id = $1;

-- name: ProfileGetByIDForUpdate :one
-- Locks the user row until the transaction ends (see ProjectsSettingsGetForUpdate).
SELECT id, username::text AS username, display_name, avatar_url, timezone, profile_public, created_at
FROM users WHERE id = $1 FOR UPDATE;

-- name: ProfileGetByUsername :one
SELECT id, username::text AS username, display_name, avatar_url, timezone, profile_public, created_at
FROM users WHERE username = sqlc.arg(username)::citext;

-- name: ProfileUpdate :one
-- Partial update: a NULL argument keeps the current value.
UPDATE users SET
    display_name   = coalesce(sqlc.narg(display_name)::text, display_name),
    timezone       = coalesce(sqlc.narg(timezone)::text, timezone),
    profile_public = coalesce(sqlc.narg(profile_public)::boolean, profile_public),
    updated_at     = now()
WHERE id = sqlc.arg(id)
RETURNING id, username::text AS username, display_name, avatar_url, timezone, profile_public, created_at;

-- name: ProfileTimezoneKnown :one
SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_timezone_names WHERE name = sqlc.arg(name)::text) AS known;

-- name: ProfilePublicTotals :many
-- Per (project, language) totals for a public profile, excluding hidden projects.
SELECT m.project, m.language, coalesce(p.repo_url, '')::text AS repo_url,
       coalesce(sum(m.seconds), 0)::bigint AS seconds,
       coalesce(sum(m.seconds) FILTER (WHERE m.minute >= sqlc.arg(since)), 0)::bigint AS recent_seconds
FROM user_minutes m
LEFT JOIN projects p ON p.user_id = m.user_id AND p.name = m.project
WHERE m.user_id = sqlc.arg(user_id)
  AND m.label = 'you'
  AND NOT coalesce(p.hidden, false)
GROUP BY m.project, m.language, p.repo_url;

-- name: StatsCreditedBreakdown :many
-- Credited time from the agent's labelled minutes in credited_minutes (see
-- ADR 0014) in [from_time, to_time), as (dimension, key, seconds) rows:
--   - credited: capped credited seconds in total (empty key);
--   - project: capped credited seconds per project;
--   - day: capped credited seconds per local day (YYYY-MM-DD);
--   - label: 60 s for every minute with any row, counted once under its
--     strongest label (you, then agent, then idle);
--   - multi_device: the number of minutes in which two or more devices
--     credited time labelled you (empty key).
-- With a project filter, only that project's rows count, but the 60-second
-- cap is still computed over every project of the minute.
WITH cp AS (
    SELECT v.minute, v.project, v.seconds
    FROM credited_project_minutes v
    WHERE v.user_id = sqlc.arg(user_id)
      AND v.minute >= sqlc.arg(from_time)
      AND v.minute < sqlc.arg(to_time)
      AND (sqlc.narg(project)::text IS NULL OR v.project = sqlc.narg(project)::text)
),
lm AS (
    SELECT c.minute,
           bool_or(c.label = 'you') AS has_you,
           bool_or(c.label = 'agent') AS has_agent,
           count(DISTINCT c.device_id) FILTER (WHERE c.label = 'you' AND c.credited > 0) AS you_devices
    FROM credited_minutes c
    WHERE c.user_id = sqlc.arg(user_id)
      AND c.minute >= sqlc.arg(from_time)
      AND c.minute < sqlc.arg(to_time)
      AND (sqlc.narg(project)::text IS NULL OR c.project = sqlc.narg(project)::text)
    GROUP BY c.minute
),
labelled AS (
    SELECT CASE WHEN lm.has_you THEN 'you' WHEN lm.has_agent THEN 'agent' ELSE 'idle' END AS label
    FROM lm
)
SELECT 'credited'::text AS dimension, ''::text AS key, coalesce(sum(cp.seconds), 0)::bigint AS seconds
FROM cp
UNION ALL
SELECT 'project'::text, cp.project::text, sum(cp.seconds)::bigint
FROM cp GROUP BY cp.project
UNION ALL
SELECT 'day'::text, to_char((cp.minute AT TIME ZONE sqlc.arg(tz)::text)::date, 'YYYY-MM-DD'), sum(cp.seconds)::bigint
FROM cp GROUP BY (cp.minute AT TIME ZONE sqlc.arg(tz)::text)::date
UNION ALL
SELECT 'label'::text, labelled.label::text, (count(*) * 60)::bigint
FROM labelled GROUP BY labelled.label
UNION ALL
SELECT 'multi_device'::text, ''::text, count(*)::bigint
FROM lm WHERE lm.you_devices >= 2;

-- name: StatsUserHasDevices :one
-- Whether the user has ever registered a device. Without one there is no
-- evidence, and stats report credited time as null rather than 0.
SELECT EXISTS (SELECT 1 FROM devices WHERE user_id = $1) AS has_devices;

-- name: ProjectsCreditedTotals :many
-- All-time capped credited seconds per project (ADR 0014), and the last
-- minute any device reported for it.
SELECT v.project, coalesce(sum(v.seconds), 0)::bigint AS seconds,
       max(v.minute)::timestamptz AS last_minute
FROM credited_project_minutes v
WHERE v.user_id = sqlc.arg(user_id)
  AND (sqlc.narg(project)::text IS NULL OR v.project = sqlc.narg(project)::text)
GROUP BY v.project;

-- name: ProfilePublicCredited :many
-- Per-project capped credited seconds for a public profile, all time and
-- since `since`, excluding hidden projects.
SELECT v.project, coalesce(p.repo_url, '')::text AS repo_url,
       coalesce(sum(v.seconds), 0)::bigint AS seconds,
       coalesce(sum(v.seconds) FILTER (WHERE v.minute >= sqlc.arg(since)), 0)::bigint AS recent_seconds
FROM credited_project_minutes v
LEFT JOIN projects p ON p.user_id = v.user_id AND p.name = v.project
WHERE v.user_id = sqlc.arg(user_id)
  AND NOT coalesce(p.hidden, false)
GROUP BY v.project, p.repo_url;
