-- Hackatime account links and history imports (ADR 0004).
-- Every query is keyed by the Logbook user id, except the trust sweep, which
-- runs as a system job over all links.

-- name: GetHackatimeLink :one
SELECT * FROM hackatime_links WHERE user_id = $1;

-- name: LockHackatimeLink :one
-- Reads the link and holds it until the transaction ends, so a concurrent
-- trust refresh can't change it between a check and a write.
SELECT * FROM hackatime_links WHERE user_id = $1 FOR UPDATE;

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
-- Applies only to the link the level was read for; a relink in the meantime
-- already stored a fresh level.
UPDATE hackatime_links SET trust_level = $2, trust_checked_at = now()
WHERE user_id = $1 AND linked_at = $3;

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

-- The importer's writes below apply only while the link the run started with
-- is still current: linked_at changes on every (re)link, so a run that was
-- overtaken by an unlink or relink can't move the new import's cursor or
-- mark it done or failed.

-- name: AdvanceHackatimeImport :one
-- Saves one imported day. prev_next_date guards against a second writer.
UPDATE hackatime_imports i
SET next_date           = sqlc.arg(next_date),
    heartbeats_imported = i.heartbeats_imported + sqlc.arg(heartbeats_imported),
    status              = 'running',
    last_error          = sqlc.arg(last_error),
    updated_at          = now()
FROM hackatime_links l
WHERE i.user_id = sqlc.arg(user_id)
  AND l.user_id = i.user_id
  AND l.linked_at = sqlc.arg(linked_at)
  AND i.next_date = sqlc.arg(prev_next_date)
RETURNING i.to_date;

-- name: FinishHackatimeImport :execrows
-- Marks the import done, unless a relink extended to_date in the meantime.
UPDATE hackatime_imports i
SET status = 'done', last_error = sqlc.arg(last_error), updated_at = now()
FROM hackatime_links l
WHERE i.user_id = sqlc.arg(user_id)
  AND l.user_id = i.user_id
  AND l.linked_at = sqlc.arg(linked_at)
  AND i.to_date = sqlc.arg(to_date);

-- name: SetHackatimeImportStatus :execrows
UPDATE hackatime_imports i
SET status = sqlc.arg(status), last_error = sqlc.arg(last_error), updated_at = now()
FROM hackatime_links l
WHERE i.user_id = sqlc.arg(user_id)
  AND l.user_id = i.user_id
  AND l.linked_at = sqlc.arg(linked_at);

-- name: ListStalledHackatimeImports :many
-- System sweep: unfinished imports that haven't moved recently.
SELECT i.user_id FROM hackatime_imports i
JOIN hackatime_links l ON l.user_id = i.user_id
WHERE i.status IN ('pending', 'running') AND i.updated_at < sqlc.arg(updated_before)
ORDER BY i.updated_at, i.user_id
LIMIT sqlc.arg(lim);

-- name: DeleteHackatimeImport :exec
DELETE FROM hackatime_imports WHERE user_id = $1;
