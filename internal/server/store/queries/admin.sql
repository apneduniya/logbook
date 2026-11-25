-- Reads for the admin API (internal/server/admin, ADR 0022). Every query
-- here is a SELECT: the admin API never changes user data, and a test
-- checks that this file holds nothing else. Its two writes (audit rows and
-- a key's last use) are in admin_keys.sql.
--
-- Heartbeats store a missing field as '', which the Hackatime-shaped calls
-- read as Hackatime's null. Durations follow Hackatime's duration_seconds:
-- each heartbeat (ordered by time, id) is credited its gap to the previous
-- one, at most 120 seconds, the first 0, and the float sum is cast to
-- integer.

-- name: AdminUsers :many
-- Users by public id, with their Hackatime link (unmasked), by public id.
SELECT u.id, u.public_id, u.username::text AS username, u.display_name, u.email, u.avatar_url,
       u.timezone, u.role, u.profile_public, u.created_at, u.updated_at,
       h.hackatime_user_id, h.hackatime_username, h.trust_level, h.trust_checked_at
FROM users u
LEFT JOIN hackatime_links h ON h.user_id = u.id
WHERE u.public_id = ANY(sqlc.arg(public_ids)::bigint[])
ORDER BY u.public_id;

-- name: AdminUserByEmail :one
-- The user with exactly this email; the oldest account when several share it.
SELECT id, public_id FROM users
WHERE email = sqlc.arg(email)
ORDER BY public_id
LIMIT 1;

-- name: AdminSearchUsers :many
-- Hackatime's fuzzy_ranked_search over public id, username, display name,
-- the GitHub username Hackatime reported and email: candidates contain the
-- term (case-insensitive) or have it as their public id; each field scores
-- 100 for an exact match, 50 for a prefix and 10 for a substring, and the
-- public id 1000; best first, then by username, at most 10. The pattern
-- arguments are LIKE-escaped by the caller.
WITH candidates AS (
    SELECT u.id, u.public_id, u.username::text AS username, u.display_name, u.email,
           coalesce(h.hackatime_username, '')::text AS github_username
    FROM users u
    LEFT JOIN hackatime_links h ON h.user_id = u.id
    WHERE u.username::text ILIKE sqlc.arg(contains)::text
       OR u.display_name ILIKE sqlc.arg(contains)::text
       OR h.hackatime_username ILIKE sqlc.arg(contains)::text
       OR u.email ILIKE sqlc.arg(contains)::text
       OR u.public_id = sqlc.narg(numeric_id)::bigint
)
SELECT c.id, c.public_id, c.username, c.display_name, c.email, c.github_username,
       (CASE WHEN c.public_id::text = sqlc.arg(exact)::text THEN 1000 ELSE 0 END
        + CASE WHEN c.username ILIKE sqlc.arg(ilike_exact)::text THEN 100
               WHEN c.username ILIKE sqlc.arg(prefix)::text THEN 50
               WHEN c.username ILIKE sqlc.arg(contains)::text THEN 10 ELSE 0 END
        + CASE WHEN c.display_name ILIKE sqlc.arg(ilike_exact)::text THEN 100
               WHEN c.display_name ILIKE sqlc.arg(prefix)::text THEN 50
               WHEN c.display_name ILIKE sqlc.arg(contains)::text THEN 10 ELSE 0 END
        + CASE WHEN c.github_username ILIKE sqlc.arg(ilike_exact)::text THEN 100
               WHEN c.github_username ILIKE sqlc.arg(prefix)::text THEN 50
               WHEN c.github_username ILIKE sqlc.arg(contains)::text THEN 10 ELSE 0 END
        + CASE WHEN c.email ILIKE sqlc.arg(ilike_exact)::text THEN 100
               WHEN c.email ILIKE sqlc.arg(prefix)::text THEN 50
               WHEN c.email ILIKE sqlc.arg(contains)::text THEN 10 ELSE 0 END)::bigint AS rank_score
FROM candidates c
ORDER BY rank_score DESC, c.username ASC
LIMIT 10;

-- name: AdminHeartbeatStats :one
-- user/info's stats over the user's heartbeats with a time from 2000 to
-- 2100, as Hackatime counts them; last_heartbeat_at is -1 without any.
WITH hb AS (
    SELECT h.id, h.time, h.language, h.project FROM heartbeats h
    WHERE h.user_id = $1
      AND h.time BETWEEN '2000-01-01T00:00:00Z'::timestamptz AND '2100-01-01T00:00:00Z'::timestamptz
),
gaps AS (
    SELECT extract(epoch FROM hb.time)::float8
           - lag(extract(epoch FROM hb.time)::float8) OVER (ORDER BY hb.time, hb.id) AS gap
    FROM hb
)
SELECT (SELECT count(*) FROM hb)::bigint AS total_heartbeats,
       (SELECT coalesce(sum(CASE WHEN g.gap IS NULL THEN 0 ELSE least(g.gap, 120) END), 0)
        FROM gaps g)::integer AS total_coding_time,
       (SELECT count(DISTINCT hb.language) FROM hb WHERE hb.language <> '')::bigint AS languages_used,
       (SELECT count(DISTINCT hb.project) FROM hb WHERE hb.project <> '')::bigint AS projects_worked_on,
       (SELECT count(DISTINCT (hb.time AT TIME ZONE 'UTC')::date) FROM hb)::bigint AS days_active,
       (SELECT coalesce(extract(epoch FROM max(hb.time))::float8, -1) FROM hb)::float8 AS last_heartbeat_at;

-- name: AdminUserCounts :one
-- The user's live API keys and devices, and their latest minute with credit.
SELECT (SELECT count(*) FROM api_keys k WHERE k.user_id = $1 AND k.revoked_at IS NULL)::bigint AS api_keys,
       (SELECT count(*) FROM devices d WHERE d.user_id = $1 AND d.revoked_at IS NULL)::bigint AS active_devices,
       (SELECT count(*) FROM devices d WHERE d.user_id = $1 AND d.revoked_at IS NOT NULL)::bigint AS revoked_devices,
       (SELECT coalesce(max(c.minute), 'epoch'::timestamptz) FROM credited_minutes c
        WHERE c.user_id = $1 AND c.credited > 0)::timestamptz AS last_credited_minute;

-- name: AdminUserHeartbeats :many
-- user/heartbeats: the user's heartbeats with Hackatime's filters, by
-- (time, id), one page. A null filter is no filter.
SELECT id, time, created_at, lineno, cursorpos, is_write, project, language, entity, branch, category,
       dependencies, editor, machine, operating_system, type, project_root_count, user_agent,
       line_additions, line_deletions, lines, source, plugin, ai_model, ai_session, ai_input_tokens,
       ai_output_tokens, ai_prompt_length, ai_line_changes, human_line_changes, fields_hash
FROM heartbeats
WHERE user_id = sqlc.arg(user_id)
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR time >= sqlc.narg(from_time)::timestamptz)
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR time <= sqlc.narg(to_time)::timestamptz)
  AND (sqlc.narg(project)::text IS NULL OR project = sqlc.narg(project)::text)
  AND (sqlc.narg(language)::text IS NULL OR language = sqlc.narg(language)::text)
  AND (sqlc.narg(entity)::text IS NULL OR entity = sqlc.narg(entity)::text)
  AND (sqlc.narg(editor)::text IS NULL OR editor = sqlc.narg(editor)::text)
  AND (sqlc.narg(machine)::text IS NULL OR machine = sqlc.narg(machine)::text)
ORDER BY time, id
LIMIT sqlc.arg(max_rows) OFFSET sqlc.arg(skip_rows);

-- name: AdminUserHeartbeatCount :one
-- How many heartbeats user/heartbeats' filters match.
SELECT count(*)::bigint
FROM heartbeats
WHERE user_id = sqlc.arg(user_id)
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR time >= sqlc.narg(from_time)::timestamptz)
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR time <= sqlc.narg(to_time)::timestamptz)
  AND (sqlc.narg(project)::text IS NULL OR project = sqlc.narg(project)::text)
  AND (sqlc.narg(language)::text IS NULL OR language = sqlc.narg(language)::text)
  AND (sqlc.narg(entity)::text IS NULL OR entity = sqlc.narg(entity)::text)
  AND (sqlc.narg(editor)::text IS NULL OR editor = sqlc.narg(editor)::text)
  AND (sqlc.narg(machine)::text IS NULL OR machine = sqlc.narg(machine)::text);

-- name: AdminUserProjects :many
-- user/projects: per named project of the user's heartbeats in [from_time,
-- to_time] (null bounds are open): heartbeats, first and last, languages,
-- Hackatime's duration within the project, the project's settings, and
-- its counted (capped credited) seconds over the same span. Most
-- heartbeats first.
WITH hb AS (
    SELECT h.id, h.time, h.project, h.language
    FROM heartbeats h
    WHERE h.user_id = sqlc.arg(user_id)
      AND h.project <> ''
      AND (sqlc.narg(from_time)::timestamptz IS NULL OR h.time >= sqlc.narg(from_time)::timestamptz)
      AND (sqlc.narg(to_time)::timestamptz IS NULL OR h.time <= sqlc.narg(to_time)::timestamptz)
),
gaps AS (
    SELECT hb.project,
           extract(epoch FROM hb.time)::float8
           - lag(extract(epoch FROM hb.time)::float8) OVER (PARTITION BY hb.project ORDER BY hb.time, hb.id) AS gap
    FROM hb
),
durations AS (
    SELECT g.project, coalesce(sum(CASE WHEN g.gap IS NULL THEN 0 ELSE least(g.gap, 120) END), 0)::integer AS duration
    FROM gaps g
    GROUP BY g.project
),
stats AS (
    SELECT hb.project, count(*)::bigint AS heartbeats,
           min(hb.time)::timestamptz AS first_time, max(hb.time)::timestamptz AS last_time,
           coalesce(array_agg(DISTINCT hb.language) FILTER (WHERE hb.language <> ''), '{}')::text[] AS languages
    FROM hb
    GROUP BY hb.project
),
credited AS (
    SELECT v.project, sum(v.seconds)::bigint AS seconds
    FROM credited_project_minutes v
    WHERE v.user_id = sqlc.arg(user_id)
      AND (sqlc.narg(from_time)::timestamptz IS NULL OR v.minute >= sqlc.narg(from_time)::timestamptz)
      AND (sqlc.narg(to_time)::timestamptz IS NULL OR v.minute <= sqlc.narg(to_time)::timestamptz)
    GROUP BY v.project
)
SELECT s.project, s.heartbeats, s.first_time, s.last_time, s.languages, d.duration,
       coalesce(p.repo_url, '')::text AS repo_url,
       coalesce(p.archived, false)::boolean AS archived,
       coalesce(p.hidden, false)::boolean AS hidden,
       coalesce(c.seconds, 0)::bigint AS credited_seconds
FROM stats s
JOIN durations d ON d.project = s.project
LEFT JOIN projects p ON p.user_id = sqlc.arg(user_id) AND p.name = s.project
LEFT JOIN credited c ON c.project = s.project
ORDER BY s.heartbeats DESC, s.project;

-- name: AdminTrustHistory :many
-- The user's newest trust readings, newest first, one more than shown so
-- the oldest shown has a previous level.
SELECT id, level, first_checked_at, checked_at FROM hackatime_trust_history
WHERE user_id = $1
ORDER BY checked_at DESC, id DESC
LIMIT 26;

-- name: AdminDevices :many
-- Every device of the user, revoked ones included, oldest first.
SELECT id, name, os, public_key, created_at, last_seen_at, revoked_at
FROM devices
WHERE user_id = $1
ORDER BY created_at, id;

-- name: AdminEvidenceDays :many
-- Every stored evidence day of the user's devices, by device and day, and
-- whether it has the block fields (ADR 0019).
SELECT e.device_id, e.day, (e.seq_to IS NOT NULL)::boolean AS has_block
FROM evidence_days e
JOIN devices d ON d.id = e.device_id
WHERE d.user_id = $1
ORDER BY e.device_id, e.day;

-- name: AdminEvidenceConflicts :many
-- Refused conflicting evidence uploads (the ledger's evidence_conflict
-- audit entries) of the given devices, oldest first.
SELECT a.id, a.subject_id AS device_id, coalesce(a.data->>'day', '')::text AS day,
       a.data::text AS data, a.created_at
FROM audit_log a
WHERE a.subject_type = 'device'
  AND a.subject_id = ANY(sqlc.arg(device_ids)::text[])
  AND a.action = 'evidence_conflict'
ORDER BY a.id;

-- name: AdminCreditedMinutes :many
-- user/minutes: the user's credited rows (every device, label and project)
-- with a minute in [from_time, to_time] (null bounds are open), one page, by
-- (minute, device, project). A null project is no filter.
SELECT device_id, minute, project, label, credited, cuts, flags, rules_version, computed_at, snapshot,
       updated_at
FROM credited_minutes
WHERE user_id = sqlc.arg(user_id)
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR minute >= sqlc.narg(from_time)::timestamptz)
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR minute <= sqlc.narg(to_time)::timestamptz)
  AND (sqlc.narg(project)::text IS NULL OR project = sqlc.narg(project)::text)
ORDER BY minute, device_id, project
LIMIT sqlc.arg(max_rows) OFFSET sqlc.arg(skip_rows);

-- name: AdminSubmissions :many
-- Submissions of one user (newest first), or one submission by id, with
-- their program's slug and name and how many parts they have.
SELECT s.id, s.user_id, s.project, s.from_day, s.to_day, s.status, s.error, s.trust_at_submission,
       s.hackatime_user_id, s.packet, s.verified_minutes, s.mismatch_minutes, s.unbacked_minutes,
       s.counted_seconds, s.raw_seconds, s.parts_version, s.created_at, s.verified_at, s.decided_at,
       s.withdrawn_at,
       coalesce(a.slug, '')::text AS program_slug, coalesce(a.name, '')::text AS program_name,
       (a.id IS NOT NULL)::boolean AS has_program,
       (SELECT count(*) FROM submission_parts p WHERE p.submission_id = s.id)::bigint AS parts,
       (SELECT count(*) FROM submission_parts p
        WHERE p.submission_id = s.id AND p.raw_deleted_at IS NOT NULL)::bigint AS parts_deleted
FROM submissions s
LEFT JOIN oauth_apps a ON a.id = s.program_app_id
WHERE (sqlc.narg(user_id)::uuid IS NULL OR s.user_id = sqlc.narg(user_id)::uuid)
  AND (sqlc.narg(id)::uuid IS NULL OR s.id = sqlc.narg(id)::uuid)
ORDER BY s.created_at DESC, s.id;

-- name: AdminSubmissionOwner :one
-- The public id and username of a submission's user.
SELECT public_id, username::text AS username FROM users WHERE id = $1;

-- name: AdminSubmissionDecisions :many
-- Every decision on the given submissions, newest first per submission,
-- with the reviewer (null once their account is gone).
SELECT d.submission_id, d.id, d.value, d.approved_seconds, d.reason, d.decided_at,
       u.public_id AS decided_by_public_id, coalesce(u.username::text, '')::text AS decided_by_username
FROM submission_decisions d
LEFT JOIN users u ON u.id = d.decided_by
WHERE d.submission_id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY d.submission_id, d.decided_at DESC, d.id DESC;

-- name: AdminSubmissionParts :many
-- A submission's parts without their data: device, size, upload and
-- deletion times.
SELECT p.device_id, d.name AS device_name, p.bytes, p.uploaded_at, p.raw_deleted_at
FROM submission_parts p
JOIN devices d ON d.id = p.device_id
WHERE p.submission_id = $1
ORDER BY p.device_id;

-- name: AdminSubmissionMinutes :many
-- A submission's frozen rows with the re-check's result, by device and minute.
SELECT device_id, minute, label, credited, cuts, flags, rules_version, computed_at, snapshot,
       result, reason, server_label, server_credited, server_cuts
FROM submission_minutes
WHERE submission_id = $1
ORDER BY device_id, minute;

-- name: AdminAuditLog :many
-- user/audit_log: entries the user acted in, or about the user, their
-- devices or their submissions, newest first, one page. subject_id is the
-- user's id as text.
SELECT a.id, a.actor_user_id, u.public_id AS actor_public_id, coalesce(u.username::text, '')::text AS actor_username,
       a.action, a.subject_type, a.subject_id, a.data::text AS data, a.created_at
FROM audit_log a
LEFT JOIN users u ON u.id = a.actor_user_id
WHERE a.id < sqlc.arg(before_id)::bigint
  AND (a.actor_user_id = sqlc.arg(user_id)::uuid
   OR (a.subject_type = 'user' AND a.subject_id = sqlc.arg(subject_id)::text)
   OR (a.subject_type = 'device'
       AND a.subject_id IN (SELECT d.id::text FROM devices d WHERE d.user_id = sqlc.arg(user_id)::uuid))
   OR (a.subject_type = 'submission'
       AND a.subject_id IN (SELECT s.id::text FROM submissions s WHERE s.user_id = sqlc.arg(user_id)::uuid)))
ORDER BY a.id DESC
LIMIT sqlc.arg(max_rows) OFFSET sqlc.arg(skip_rows);

-- name: AdminAuditNextID :one
-- The id the next audit row will be above: user/audit_log pages are read
-- below it, so rows written while paging (each page's own admin.read rows
-- among them) never shift later pages.
SELECT (coalesce(max(id), 0) + 1)::bigint AS next_id FROM audit_log;
