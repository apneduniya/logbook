-- Queries for internal/server/oauth: apps and their members, consent
-- requests, grants, codes and tokens (ADR 0021). Reads of a user's data are
-- scoped to that user_id; writes to an app are scoped to its owner_id.

-- name: OAuthAppCountOwned :one
SELECT count(*) FROM oauth_apps WHERE owner_id = $1 AND deleted_at IS NULL;

-- name: OAuthAppInsert :one
-- Returns no row when the slug is taken (by any app, deleted ones included).
INSERT INTO oauth_apps (owner_id, name, slug, redirect_uris, homepage_url, public, secret_hash,
                        secret_rotated_at, created_at, updated_at)
VALUES (sqlc.arg(owner_id), sqlc.arg(name), sqlc.arg(slug), sqlc.arg(redirect_uris)::text[],
        sqlc.arg(homepage_url), sqlc.arg(public), sqlc.narg(secret_hash),
        sqlc.narg(secret_rotated_at)::timestamptz, sqlc.arg(created_at), sqlc.arg(created_at))
ON CONFLICT (slug) DO NOTHING
RETURNING *;

-- name: OAuthAppByID :one
-- A live app with its owner's username, for client authentication and
-- the authorize endpoint.
SELECT a.*, u.username AS owner_username
FROM oauth_apps a
JOIN users u ON u.id = a.owner_id
WHERE a.id = $1 AND a.deleted_at IS NULL;

-- name: OAuthAppForMember :one
-- A live app as one of its members sees it. No row: no such app, deleted,
-- or the user isn't a member.
SELECT a.*, m.role, o.username AS owner_username
FROM oauth_apps a
JOIN app_members m ON m.app_id = a.id AND m.user_id = sqlc.arg(user_id)
JOIN users o ON o.id = a.owner_id
WHERE a.id = sqlc.arg(id) AND a.deleted_at IS NULL;

-- name: OAuthAppsForMember :many
-- Live apps the user owns or reviews for, newest first.
SELECT a.*, m.role, o.username AS owner_username
FROM oauth_apps a
JOIN app_members m ON m.app_id = a.id AND m.user_id = sqlc.arg(user_id)
JOIN users o ON o.id = a.owner_id
WHERE a.deleted_at IS NULL
ORDER BY a.created_at DESC, a.id;

-- name: OAuthAppLockOwned :one
-- Locks a live app owned by owner_id for the rest of the transaction.
SELECT id FROM oauth_apps
WHERE id = sqlc.arg(id) AND owner_id = sqlc.arg(owner_id) AND deleted_at IS NULL
FOR UPDATE;

-- name: OAuthAppUpdate :one
UPDATE oauth_apps
SET name = sqlc.arg(name), redirect_uris = sqlc.arg(redirect_uris)::text[],
    homepage_url = sqlc.arg(homepage_url), public = sqlc.arg(public),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND owner_id = sqlc.arg(owner_id) AND deleted_at IS NULL
RETURNING *;

-- name: OAuthAppSetSecret :one
-- Replaces a confidential app's secret. No row for public apps.
UPDATE oauth_apps
SET secret_hash = sqlc.arg(secret_hash), secret_rotated_at = sqlc.arg(rotated_at)::timestamptz,
    updated_at = sqlc.arg(rotated_at)::timestamptz
WHERE id = sqlc.arg(id) AND owner_id = sqlc.arg(owner_id) AND deleted_at IS NULL
  AND secret_hash IS NOT NULL
RETURNING *;

-- name: OAuthAppDelete :one
UPDATE oauth_apps
SET deleted_at = sqlc.arg(deleted_at)::timestamptz, public = false, updated_at = sqlc.arg(deleted_at)::timestamptz
WHERE id = sqlc.arg(id) AND owner_id = sqlc.arg(owner_id) AND deleted_at IS NULL
RETURNING *;

-- name: PublicPrograms :many
SELECT slug, name, homepage_url FROM oauth_apps
WHERE public AND deleted_at IS NULL
ORDER BY lower(name), slug;

-- name: AppMemberInsert :execrows
-- Adds a member. 0 rows: already a member.
INSERT INTO app_members (app_id, user_id, role, added_by, created_at)
VALUES (sqlc.arg(app_id), sqlc.arg(user_id), sqlc.arg(role), sqlc.narg(added_by), sqlc.arg(created_at))
ON CONFLICT (app_id, user_id) DO NOTHING;

-- name: AppMembers :many
-- An app's members, owner first, then reviewers in the order they were added.
SELECT u.id AS user_id, u.username, m.role, m.created_at, a.username AS added_by_username
FROM app_members m
JOIN users u ON u.id = m.user_id
LEFT JOIN users a ON a.id = m.added_by
WHERE m.app_id = $1
ORDER BY m.role = 'owner' DESC, m.created_at, u.username;

-- name: AppReviewerCount :one
SELECT count(*) FROM app_members WHERE app_id = $1 AND role = 'reviewer';

-- name: AppReviewerDelete :one
-- Removes a reviewer. The owner can't be removed. No row: not a reviewer.
DELETE FROM app_members
WHERE app_id = sqlc.arg(app_id) AND user_id = sqlc.arg(user_id) AND role = 'reviewer'
RETURNING user_id;

-- name: OAuthRequestInsert :exec
INSERT INTO oauth_requests (id_hash, app_id, user_id, redirect_uri, scopes, state,
                            code_challenge, code_challenge_method, created_at, expires_at)
VALUES (sqlc.arg(id_hash), sqlc.arg(app_id), sqlc.narg(user_id), sqlc.arg(redirect_uri),
        sqlc.arg(scopes)::text[], sqlc.arg(state), sqlc.arg(code_challenge),
        sqlc.arg(code_challenge_method), sqlc.arg(created_at), sqlc.arg(expires_at));

-- name: OAuthRequestClaim :one
-- Returns an unexpired pending request for user_id, binding it to that user
-- if nobody has opened it yet. No row: unknown, expired or another user's.
UPDATE oauth_requests
SET user_id = sqlc.arg(user_id)
WHERE id_hash = sqlc.arg(id_hash) AND expires_at > sqlc.arg(now)::timestamptz
  AND (user_id IS NULL OR user_id = sqlc.arg(user_id))
RETURNING *;

-- name: OAuthRequestConsume :one
-- Removes and returns a pending request, as OAuthRequestClaim matches it.
DELETE FROM oauth_requests
WHERE id_hash = sqlc.arg(id_hash) AND expires_at > sqlc.arg(now)::timestamptz
  AND (user_id IS NULL OR user_id = sqlc.arg(user_id))
RETURNING *;

-- name: OAuthGrantForUpdate :one
SELECT * FROM oauth_grants WHERE user_id = $1 AND app_id = $2 FOR UPDATE;

-- name: OAuthGrantForShare :one
-- Locks the grant against revocation (whose UPDATE waits on it) while a
-- token is issued from it, without blocking other readers or refreshes.
SELECT * FROM oauth_grants WHERE user_id = $1 AND app_id = $2 FOR SHARE;

-- name: OAuthGrantActive :one
SELECT * FROM oauth_grants WHERE user_id = $1 AND app_id = $2 AND revoked_at IS NULL;

-- name: OAuthGrantUpsert :exec
INSERT INTO oauth_grants (user_id, app_id, scopes, created_at, updated_at)
VALUES (sqlc.arg(user_id), sqlc.arg(app_id), sqlc.arg(scopes)::text[], sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (user_id, app_id) DO UPDATE
    SET scopes = EXCLUDED.scopes, updated_at = EXCLUDED.updated_at, revoked_at = NULL,
        created_at = CASE WHEN oauth_grants.revoked_at IS NULL THEN oauth_grants.created_at
                          ELSE EXCLUDED.created_at END;

-- name: OAuthGrantsForUser :many
-- The user's active grants to live apps, newest first.
SELECT g.app_id, g.scopes, g.created_at, g.updated_at,
       a.name AS app_name, a.slug AS app_slug, a.homepage_url, o.username AS owner_username
FROM oauth_grants g
JOIN oauth_apps a ON a.id = g.app_id AND a.deleted_at IS NULL
JOIN users o ON o.id = a.owner_id
WHERE g.user_id = $1 AND g.revoked_at IS NULL
ORDER BY g.updated_at DESC, g.app_id;

-- name: OAuthGrantRevoke :one
UPDATE oauth_grants SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE user_id = sqlc.arg(user_id) AND app_id = sqlc.arg(app_id) AND revoked_at IS NULL
RETURNING *;

-- name: OAuthGrantsRevokeApp :exec
UPDATE oauth_grants SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE app_id = sqlc.arg(app_id) AND revoked_at IS NULL;

-- name: OAuthCodeInsert :exec
INSERT INTO oauth_codes (code_hash, app_id, user_id, redirect_uri, scopes, code_challenge,
                         code_challenge_method, family_id, created_at, expires_at)
VALUES (sqlc.arg(code_hash), sqlc.arg(app_id), sqlc.arg(user_id), sqlc.arg(redirect_uri),
        sqlc.arg(scopes)::text[], sqlc.arg(code_challenge), sqlc.arg(code_challenge_method),
        sqlc.arg(family_id), sqlc.arg(created_at), sqlc.arg(expires_at));

-- name: OAuthCodeForUpdate :one
SELECT * FROM oauth_codes WHERE code_hash = $1 FOR UPDATE;

-- name: OAuthCodeMarkUsed :exec
UPDATE oauth_codes SET used_at = sqlc.arg(used_at)::timestamptz
WHERE code_hash = sqlc.arg(code_hash) AND used_at IS NULL;

-- name: OAuthCodesBurnForGrant :exec
-- Marks the user's unused codes for the app as used, so none can be exchanged.
UPDATE oauth_codes SET used_at = sqlc.arg(used_at)::timestamptz
WHERE user_id = sqlc.arg(user_id) AND app_id = sqlc.arg(app_id) AND used_at IS NULL;

-- name: OAuthCodesBurnForApp :exec
UPDATE oauth_codes SET used_at = sqlc.arg(used_at)::timestamptz
WHERE app_id = sqlc.arg(app_id) AND used_at IS NULL;

-- name: OAuthRequestsDeleteForApp :exec
DELETE FROM oauth_requests WHERE app_id = $1;

-- name: OAuthTokenInsert :one
INSERT INTO oauth_tokens (access_hash, refresh_hash, app_id, user_id, scopes, family_id,
                          created_at, expires_at, refresh_expires_at)
VALUES (sqlc.arg(access_hash), sqlc.narg(refresh_hash), sqlc.arg(app_id), sqlc.narg(user_id),
        sqlc.arg(scopes)::text[], sqlc.arg(family_id), sqlc.arg(created_at), sqlc.arg(expires_at),
        sqlc.narg(refresh_expires_at)::timestamptz)
RETURNING id;

-- name: OAuthTokenByAccess :one
-- A usable access token: unrevoked, unexpired, of a live app. username and
-- role are null for client-credentials tokens.
SELECT t.id, t.app_id, t.user_id, t.scopes, u.username, u.role
FROM oauth_tokens t
JOIN oauth_apps a ON a.id = t.app_id AND a.deleted_at IS NULL
LEFT JOIN users u ON u.id = t.user_id
WHERE t.access_hash = sqlc.arg(access_hash) AND t.revoked_at IS NULL
  AND t.expires_at > sqlc.arg(now)::timestamptz;

-- name: OAuthTokenOwnerByRefresh :one
-- The app, user and family of a refresh token, read without a lock so the
-- grant and the family can be locked before the token.
SELECT app_id, user_id, family_id FROM oauth_tokens WHERE refresh_hash = $1;

-- name: OAuthFamilyLock :exec
-- Serialises issuing a token into a family with revoking the family, until
-- the transaction ends: a revocation that waited here then sees every token
-- the issuer added. 1002 namespaces the lock (ingest uses 1001).
SELECT pg_advisory_xact_lock(1002, hashtext(sqlc.arg(family_id)::uuid::text));

-- name: OAuthTokenByRefreshForUpdate :one
SELECT * FROM oauth_tokens WHERE refresh_hash = $1 FOR UPDATE;

-- name: OAuthTokenRevoke :exec
UPDATE oauth_tokens SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: OAuthTokenRevokeByHash :one
-- Revokes the app's token whose access or refresh token hashes to hash.
-- No row: unknown, another app's, or already revoked.
UPDATE oauth_tokens SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE app_id = sqlc.arg(app_id) AND (access_hash = sqlc.arg(hash) OR refresh_hash = sqlc.arg(hash))
  AND revoked_at IS NULL
RETURNING id, user_id;

-- name: OAuthTokensRevokeFamily :execrows
UPDATE oauth_tokens SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE family_id = sqlc.arg(family_id) AND revoked_at IS NULL;

-- name: OAuthTokensRevokeGrant :execrows
UPDATE oauth_tokens SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE user_id = sqlc.arg(user_id) AND app_id = sqlc.arg(app_id) AND revoked_at IS NULL;

-- name: OAuthTokensRevokeApp :execrows
UPDATE oauth_tokens SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE app_id = sqlc.arg(app_id) AND revoked_at IS NULL;

-- name: OAuthDeleteExpiredRequests :execrows
DELETE FROM oauth_requests WHERE expires_at <= $1;

-- name: OAuthDeleteExpiredCodes :execrows
DELETE FROM oauth_codes WHERE expires_at <= $1;

-- name: OAuthDeleteDeadTokens :execrows
-- Tokens that can no longer be used: both halves expired, or revoked.
DELETE FROM oauth_tokens
WHERE (expires_at <= sqlc.arg(before)::timestamptz
       AND (refresh_expires_at IS NULL OR refresh_expires_at <= sqlc.arg(before)::timestamptz))
   OR revoked_at <= sqlc.arg(before)::timestamptz;
