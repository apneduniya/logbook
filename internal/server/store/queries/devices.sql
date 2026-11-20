-- Queries for internal/server/devices, internal/server/credited and
-- internal/server/ledger. Every query is scoped to one user_id or to a
-- device the caller has already checked belongs to that user.

-- name: DeviceInsert :one
-- Registers a device. Returns no row when the public key is already
-- registered (to anyone); the caller then reads the existing row.
INSERT INTO devices (user_id, name, os, public_key)
VALUES (sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(os), sqlc.arg(public_key))
ON CONFLICT (public_key) DO NOTHING
RETURNING *;

-- name: DeviceByPublicKey :one
SELECT * FROM devices WHERE public_key = $1;

-- name: DeviceCountActive :one
SELECT count(*) FROM devices WHERE user_id = $1 AND revoked_at IS NULL;

-- name: DeviceList :many
-- All of a user's devices, revoked ones included, newest first.
SELECT * FROM devices WHERE user_id = $1 ORDER BY created_at DESC, id;

-- name: DeviceRevoke :one
-- Revokes one of the user's active devices. No row means it doesn't exist,
-- belongs to someone else or is already revoked.
UPDATE devices SET revoked_at = sqlc.arg(revoked_at)::timestamptz
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND revoked_at IS NULL
RETURNING *;

-- name: DeviceLockForUpload :one
-- Locks one of the user's devices until the transaction ends, so uploads for
-- a device are serialised (the 60-second rule and the evidence chain depend
-- on it) and a concurrent revoke waits for them. No row means the device
-- doesn't exist or belongs to someone else.
SELECT * FROM devices
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
FOR NO KEY UPDATE;

-- name: DeviceTouch :exec
-- Records upload activity, at most once a minute per device.
UPDATE devices SET last_seen_at = sqlc.arg(seen_at)::timestamptz
WHERE id = sqlc.arg(id)
  AND (last_seen_at IS NULL OR last_seen_at < sqlc.arg(seen_at)::timestamptz - interval '1 minute');

-- name: CreditedMinutesForDevice :many
-- The device's stored rows for the given minutes, for the rewrite-window and
-- 60-second checks.
SELECT minute, project, label, credited, cuts, flags, rules_version
FROM credited_minutes
WHERE device_id = sqlc.arg(device_id)
  AND minute = ANY(sqlc.arg(minutes)::timestamptz[])
ORDER BY minute, project;

-- name: CreditedMinutesUpsert :exec
-- Writes a checked batch in one round trip. rows is a JSON array of objects
-- with minute (RFC 3339), project, label, credited, cuts and flags. Unchanged
-- rows are skipped so retries produce no dead tuples.
INSERT INTO credited_minutes (
    device_id, user_id, minute, project, label, credited, cuts, flags, rules_version, updated_at
)
SELECT sqlc.arg(device_id)::uuid, sqlc.arg(user_id)::uuid, r.minute, r.project, r.label,
       r.credited, coalesce(r.cuts, '{}'), coalesce(r.flags, '{}'),
       sqlc.arg(rules_version)::text, sqlc.arg(updated_at)::timestamptz
FROM jsonb_to_recordset(sqlc.arg(rows)::jsonb) AS r(
    minute timestamptz, project text, label text, credited smallint, cuts text[], flags text[]
)
ON CONFLICT (device_id, minute, project) DO UPDATE SET
    label         = EXCLUDED.label,
    credited      = EXCLUDED.credited,
    cuts          = EXCLUDED.cuts,
    flags         = EXCLUDED.flags,
    rules_version = EXCLUDED.rules_version,
    updated_at    = EXCLUDED.updated_at
WHERE (credited_minutes.label, credited_minutes.credited, credited_minutes.cuts,
       credited_minutes.flags, credited_minutes.rules_version)
      IS DISTINCT FROM
      (EXCLUDED.label, EXCLUDED.credited, EXCLUDED.cuts, EXCLUDED.flags, EXCLUDED.rules_version);

-- name: EvidenceDayGet :one
SELECT * FROM evidence_days WHERE device_id = $1 AND day = $2;

-- name: EvidenceDayNeighbours :one
-- The chain hash of the device's latest stored day before day (empty if
-- none), and whether a later day is stored and, if so, the prev_chain_hash
-- of the earliest one.
SELECT
    coalesce((SELECT e.chain_hash FROM evidence_days e
              WHERE e.device_id = sqlc.arg(device_id) AND e.day < sqlc.arg(day)
              ORDER BY e.day DESC LIMIT 1), '')::text AS before_chain_hash,
    EXISTS (SELECT 1 FROM evidence_days e
            WHERE e.device_id = sqlc.arg(device_id) AND e.day > sqlc.arg(day)) AS has_after,
    coalesce((SELECT e.prev_chain_hash FROM evidence_days e
              WHERE e.device_id = sqlc.arg(device_id) AND e.day > sqlc.arg(day)
              ORDER BY e.day LIMIT 1), '')::text AS after_prev_chain_hash;

-- name: EvidenceDayInsert :exec
INSERT INTO evidence_days (
    device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version, event_counts
) VALUES (
    sqlc.arg(device_id), sqlc.arg(day), sqlc.arg(prev_chain_hash), sqlc.arg(content_hash),
    sqlc.arg(chain_hash), sqlc.arg(signature), sqlc.arg(rules_version), sqlc.arg(event_counts)
);
