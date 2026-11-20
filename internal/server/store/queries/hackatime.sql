-- Hackatime account links and history imports (ADR 0004).
-- Every query is keyed by the Logbook user id, except the trust sweep, which
-- runs as a system job over all links.

-- name: GetHackatimeLink :one
SELECT * FROM hackatime_links WHERE user_id = $1;

-- name: GetHackatimeLinkOwner :one
SELECT user_id FROM hackatime_links WHERE hackatime_user_id = $1;

-- name: UpsertHackatimeLink :one
INSERT INTO hackatime_links (user_id, hackatime_user_id, hackatime_username, access_token_enc,
                             trust_level, trust_checked_at, linked_at)
VALUES ($1, $2, $3, $4, $5, now(), now())
ON CONFLICT (user_id) DO UPDATE SET
    hackatime_user_id  = EXCLUDED.hackatime_user_id,
    hackatime_username = EXCLUDED.hackatime_username,
    access_token_enc   = EXCLUDED.access_token_enc,
    trust_level        = EXCLUDED.trust_level,
    trust_checked_at   = EXCLUDED.trust_checked_at,
    linked_at          = EXCLUDED.linked_at
RETURNING *;

-- name: DeleteHackatimeLink :one
DELETE FROM hackatime_links WHERE user_id = $1 RETURNING *;

-- name: UpdateHackatimeTrust :execrows
UPDATE hackatime_links SET trust_level = $2, trust_checked_at = now() WHERE user_id = $1;

-- name: ListHackatimeLinksDueForTrustCheck :many
SELECT user_id FROM hackatime_links
WHERE trust_checked_at IS NULL OR trust_checked_at < $1
ORDER BY trust_checked_at NULLS FIRST, user_id
LIMIT $2;

-- name: GetHackatimeImport :one
SELECT * FROM hackatime_imports WHERE user_id = $1;

-- name: StartHackatimeImport :one
-- Creates the import, or extends an existing one to a new end date and makes
-- it runnable again. next_date is kept so finished days aren't refetched.
INSERT INTO hackatime_imports (user_id, status, from_date, to_date, next_date)
VALUES ($1, 'pending', $2, $3, $2)
ON CONFLICT (user_id) DO UPDATE SET
    status     = 'pending',
    to_date    = GREATEST(hackatime_imports.to_date, EXCLUDED.to_date),
    last_error = '',
    updated_at = now()
RETURNING *;

-- name: AdvanceHackatimeImport :execrows
UPDATE hackatime_imports
SET next_date = $2,
    heartbeats_imported = heartbeats_imported + $3,
    status = 'running',
    last_error = '',
    updated_at = now()
WHERE user_id = $1;

-- name: SetHackatimeImportStatus :exec
UPDATE hackatime_imports
SET status = $2, last_error = $3, updated_at = now()
WHERE user_id = $1;

-- name: DeleteHackatimeImport :exec
DELETE FROM hackatime_imports WHERE user_id = $1;
