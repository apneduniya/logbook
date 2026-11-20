-- name: InsertSignInUser :one
-- Creates a user on first sign-in. A concurrent first sign-in of the same
-- identity updates the profile instead of failing.
INSERT INTO users (hca_id, username, display_name, email)
VALUES ($1, $2, $3, $4)
ON CONFLICT (hca_id) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        email        = EXCLUDED.email,
        updated_at   = now()
RETURNING *;

-- name: UpdateSignInProfile :one
UPDATE users
SET display_name = $2, email = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UsernameExists :one
SELECT EXISTS (SELECT 1 FROM users WHERE username = $1);

-- name: LockUser :exec
SELECT 1 FROM users WHERE id = $1 FOR UPDATE;

-- name: CreateSession :exec
INSERT INTO sessions (id_hash, user_id, csrf_token, created_at, expires_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $4);

-- name: GetActiveSession :one
SELECT s.id_hash, s.user_id, s.csrf_token, s.expires_at, s.last_seen_at,
       u.username, u.role
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id_hash = $1 AND s.expires_at > $2;

-- name: TouchSession :execrows
UPDATE sessions
SET last_seen_at = sqlc.arg(seen_at), expires_at = sqlc.arg(expires_at)
WHERE id_hash = sqlc.arg(id_hash) AND last_seen_at < sqlc.arg(touch_before)::timestamptz;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id_hash = $1;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= $1;

-- name: CountActiveAPIKeys :one
SELECT count(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL;

-- name: CreateAPIKey :one
INSERT INTO api_keys (user_id, name, key_hash, key_prefix, created_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListActiveAPIKeys :many
SELECT * FROM api_keys
WHERE user_id = $1 AND revoked_at IS NULL
ORDER BY created_at DESC, id;

-- name: RevokeAPIKey :one
UPDATE api_keys
SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
RETURNING *;

-- name: GetActiveAPIKeyByHash :one
SELECT k.id, k.user_id, k.last_used_at, u.username, u.role
FROM api_keys k
JOIN users u ON u.id = k.user_id
WHERE k.key_hash = $1 AND k.revoked_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys
SET last_used_at = sqlc.arg(used_at)::timestamptz
WHERE id = sqlc.arg(id) AND (last_used_at IS NULL OR last_used_at < sqlc.arg(touch_before)::timestamptz);
