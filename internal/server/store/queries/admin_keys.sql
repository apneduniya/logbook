-- Queries for internal/server/admin that write (ADR 0022): the admin role
-- and admin keys, which only the server's command line changes, and the
-- two writes an admin API request makes: its audit rows and its key's
-- last_used_at. Every read the admin API serves is in admin.sql.

-- name: AdminUserForUpdate :one
-- Locks a user by username until the transaction ends, so a role change
-- and a key creation for that user never interleave.
SELECT id, username::text AS username, role FROM users
WHERE username = sqlc.arg(username)::citext
FOR UPDATE;

-- name: AdminSetRole :exec
UPDATE users SET role = sqlc.arg(role) WHERE id = sqlc.arg(id);

-- name: AdminKeyInsert :one
-- Stores a new key. No row when the prefix is taken; the caller makes
-- another key.
INSERT INTO admin_keys (user_id, name, prefix, key_hash, created_at)
VALUES (sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(prefix), sqlc.arg(key_hash), sqlc.arg(created_at)::timestamptz)
ON CONFLICT (prefix) DO NOTHING
RETURNING id;

-- name: AdminKeysRevokeOfUser :execrows
-- Revokes every live key of a user.
UPDATE admin_keys SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL;

-- name: AdminKeyRevoke :one
-- Revokes one live key by its prefix. No row when no live key has it.
UPDATE admin_keys k SET revoked_at = sqlc.arg(revoked_at)::timestamptz
FROM users u
WHERE k.prefix = sqlc.arg(prefix) AND k.revoked_at IS NULL AND u.id = k.user_id
RETURNING k.id, k.name, k.prefix, u.username::text AS username;

-- name: AdminKeyByPrefix :one
-- A key with its owner's role, revoked or not: authentication checks the
-- hash, the revocation and the role.
SELECT k.id, k.user_id, k.name, k.prefix, k.key_hash, k.created_at, k.last_used_at, k.revoked_at,
       u.username::text AS username, u.display_name, u.public_id, u.role
FROM admin_keys k
JOIN users u ON u.id = k.user_id
WHERE k.prefix = $1;

-- name: AdminKeysList :many
-- Every key, revoked ones included, oldest first.
SELECT k.id, k.name, k.prefix, k.created_at, k.last_used_at, k.revoked_at,
       u.username::text AS username, u.role
FROM admin_keys k
JOIN users u ON u.id = k.user_id
ORDER BY k.created_at, k.id;

-- name: AdminKeyTouch :exec
-- Records a key's use, at most once a minute.
UPDATE admin_keys SET last_used_at = sqlc.arg(used_at)::timestamptz
WHERE id = sqlc.arg(id)
  AND (last_used_at IS NULL OR last_used_at < sqlc.arg(touch_before)::timestamptz);

-- name: AdminAuditReads :exec
-- One admin.read audit row per subject of an admin API request: the users
-- it read (subject_type 'user'), or the key itself when it read no user
-- (subject_type 'admin_key').
INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id, data)
SELECT sqlc.arg(actor_user_id)::uuid, 'admin.read', sqlc.arg(subject_type)::text, s.id, sqlc.arg(data)::jsonb
FROM unnest(sqlc.arg(subject_ids)::text[]) AS s(id);

-- name: AdminAuditCommand :exec
-- An audit row for a server command-line action (no actor).
INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id, data)
VALUES (NULL, sqlc.arg(action), sqlc.arg(subject_type), sqlc.arg(subject_id), sqlc.arg(data)::jsonb);
