-- Test fixtures for package store/seedtest. Production code must not call
-- these; creating users belongs to auth and writing minutes to ingest.

-- name: SeedTestUser :one
INSERT INTO users (hca_id, username, display_name, email, timezone, profile_public)
VALUES (sqlc.arg(hca_id), sqlc.arg(username), sqlc.arg(display_name), sqlc.arg(email),
        sqlc.arg(timezone), sqlc.arg(profile_public))
RETURNING id;

-- name: SeedTestUserMinute :exec
INSERT INTO user_minutes (user_id, minute, project, language, editor, category, label, seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: SeedTestAuditLog :many
-- Audit rows written by one actor, oldest first.
SELECT action, subject_type, subject_id, data::text AS data
FROM audit_log
WHERE actor_user_id = $1
ORDER BY id;

-- name: SeedTestDevice :one
INSERT INTO devices (user_id, name, os, public_key, revoked_at, created_at)
VALUES (sqlc.arg(user_id), sqlc.arg(name), 'linux', sqlc.arg(public_key), sqlc.narg(revoked_at),
        sqlc.arg(created_at)::timestamptz)
RETURNING id;

-- name: SeedTestCreditedMinute :exec
INSERT INTO credited_minutes (device_id, user_id, minute, project, label, credited, flags, rules_version)
VALUES (sqlc.arg(device_id), sqlc.arg(user_id), sqlc.arg(minute), sqlc.arg(project), sqlc.arg(label),
        sqlc.arg(credited), sqlc.arg(flags), 'v1');

-- name: SeedTestHeartbeat :exec
INSERT INTO heartbeats (user_id, time, fields_hash, source, entity, category, project, language, editor)
VALUES (sqlc.arg(user_id), sqlc.arg(time), sqlc.arg(fields_hash), 'editor', sqlc.arg(entity),
        sqlc.arg(category), sqlc.arg(project), sqlc.arg(language), sqlc.arg(editor));

-- name: SeedTestHackatimeLink :exec
INSERT INTO hackatime_links (user_id, hackatime_user_id, hackatime_username, access_token_enc,
                             trust_level, trust_checked_at)
VALUES (sqlc.arg(user_id), sqlc.arg(hackatime_user_id), sqlc.arg(hackatime_username), '\x00',
        sqlc.arg(trust_level), now());

-- name: SeedTestProjectSettings :exec
INSERT INTO projects (user_id, name, repo_url, hidden, archived)
VALUES (sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(repo_url), sqlc.arg(hidden), sqlc.arg(archived))
ON CONFLICT (user_id, name) DO UPDATE SET
    repo_url = EXCLUDED.repo_url, hidden = EXCLUDED.hidden, archived = EXCLUDED.archived;

-- name: SeedTestPublicID :one
SELECT public_id FROM users WHERE id = $1;
