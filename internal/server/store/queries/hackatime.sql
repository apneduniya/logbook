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
-- it runnable again. Imported days are [from_date, next_date) and
-- [recent_from, recent_next); the importer fills the recent segment up to
-- to_date first, then backfills from next_date up to recent_from.
--
-- On an extension, finished days aren't refetched. The recent segment carries
-- on from recent_next when it still reaches the new recent start. Otherwise a
-- new recent segment starts at the new recent start: if the backfill had
-- already joined the old segment, everything before the old recent_next is
-- done and becomes the backfill cursor; if not, the old segment is dropped
-- (its days are fetched again by the backfill, and ingest drops duplicates).
INSERT INTO hackatime_imports AS i (user_id, status, from_date, to_date, next_date, recent_from, recent_next)
VALUES (sqlc.arg(user_id), 'pending', sqlc.arg(from_date), sqlc.arg(to_date), sqlc.arg(from_date),
        sqlc.arg(recent_from), sqlc.arg(recent_from))
ON CONFLICT (user_id) DO UPDATE SET
    status      = 'pending',
    to_date     = GREATEST(i.to_date, EXCLUDED.to_date),
    next_date   = CASE WHEN i.recent_next < EXCLUDED.recent_from AND i.next_date = i.recent_from
                       THEN i.recent_next ELSE i.next_date END,
    recent_from = CASE WHEN i.recent_next < EXCLUDED.recent_from
                       THEN EXCLUDED.recent_from ELSE i.recent_from END,
    recent_next = CASE WHEN i.recent_next < EXCLUDED.recent_from
                       THEN EXCLUDED.recent_from ELSE i.recent_next END,
    recent_done = false,
    last_error  = '',
    updated_at  = now()
RETURNING *;

-- The importer's writes below apply only while the link the run started with
-- is still current: linked_at changes on every (re)link, so a run that was
-- overtaken by an unlink or relink can't move the new import's cursors or
-- mark it done or failed.

-- name: AdvanceHackatimeRecent :execrows
-- Saves progress in the recent segment: adds the heartbeats a finished window
-- stored and moves recent_next (it may stay put when an earlier window is
-- still in flight). prev_recent_next guards against a second writer. While
-- the backfill has nothing left before the recent segment, next_date and
-- recent_from move with it.
UPDATE hackatime_imports i
SET recent_next         = sqlc.arg(recent_next),
    next_date           = CASE WHEN i.next_date = i.recent_from THEN sqlc.arg(recent_next) ELSE i.next_date END,
    recent_from         = CASE WHEN i.next_date = i.recent_from THEN sqlc.arg(recent_next) ELSE i.recent_from END,
    recent_done         = sqlc.arg(recent_done),
    heartbeats_imported = i.heartbeats_imported + sqlc.arg(heartbeats_imported),
    status              = 'running',
    last_error          = sqlc.arg(last_error),
    updated_at          = now()
FROM hackatime_links l
WHERE i.user_id = sqlc.arg(user_id)
  AND l.user_id = i.user_id
  AND l.linked_at = sqlc.arg(linked_at)
  AND i.recent_next = sqlc.arg(prev_recent_next)
  AND NOT i.recent_done;

-- name: AdvanceHackatimeBackfill :execrows
-- Saves backfill progress, like AdvanceHackatimeRecent. Once next_date
-- reaches recent_from the two segments join, and all three cursors move to
-- recent_next.
UPDATE hackatime_imports i
SET next_date           = CASE WHEN sqlc.arg(next_date)::date >= i.recent_from THEN i.recent_next ELSE sqlc.arg(next_date) END,
    recent_from         = CASE WHEN sqlc.arg(next_date)::date >= i.recent_from THEN i.recent_next ELSE i.recent_from END,
    heartbeats_imported = i.heartbeats_imported + sqlc.arg(heartbeats_imported),
    status              = 'running',
    last_error          = sqlc.arg(last_error),
    updated_at          = now()
FROM hackatime_links l
WHERE i.user_id = sqlc.arg(user_id)
  AND l.user_id = i.user_id
  AND l.linked_at = sqlc.arg(linked_at)
  AND i.next_date = sqlc.arg(prev_next_date)
  AND i.recent_done;

-- name: FinishHackatimeImport :execrows
-- Marks the import done, unless a relink extended to_date in the meantime.
UPDATE hackatime_imports i
SET status = 'done', last_error = sqlc.arg(last_error), updated_at = now()
FROM hackatime_links l
WHERE i.user_id = sqlc.arg(user_id)
  AND l.user_id = i.user_id
  AND l.linked_at = sqlc.arg(linked_at)
  AND i.to_date = sqlc.arg(to_date)
  AND i.recent_done
  AND i.next_date = i.recent_from;

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

-- name: RecordHackatimeTrust :exec
-- Adds a level read for the user to hackatime_trust_history: when the newest
-- row has the same level, its checked_at moves forward; otherwise (a change,
-- or the first read) a new row starts.
WITH latest AS (
    SELECT id, level FROM hackatime_trust_history
    WHERE user_id = sqlc.arg(user_id)
    ORDER BY checked_at DESC, id DESC
    LIMIT 1
),
moved AS (
    UPDATE hackatime_trust_history h SET checked_at = sqlc.arg(checked_at)
    FROM latest
    WHERE h.id = latest.id AND latest.level = sqlc.arg(level)::text
    RETURNING h.id
)
INSERT INTO hackatime_trust_history (user_id, level, checked_at)
SELECT sqlc.arg(user_id), sqlc.arg(level)::text, sqlc.arg(checked_at)
WHERE NOT EXISTS (SELECT 1 FROM moved);

-- name: ListHackatimeTrustHistory :many
-- The user's newest trust readings, newest first.
SELECT level, checked_at FROM hackatime_trust_history
WHERE user_id = $1
ORDER BY checked_at DESC, id DESC
LIMIT $2;
