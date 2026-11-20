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
