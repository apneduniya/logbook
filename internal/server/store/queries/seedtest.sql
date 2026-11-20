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
