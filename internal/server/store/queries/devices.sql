-- Queries for internal/server/devices, internal/server/credited and
-- internal/server/ledger. Every query is scoped to one user_id or to a
-- device the caller has already checked belongs to that user.

-- name: DeviceInsert :one
-- Registers a device. Returns no row when the public key is already
-- registered (to anyone); the caller then reads the existing row.
-- created_at comes from the service clock: uploads are only accepted from
-- the UTC day it falls on.
INSERT INTO devices (user_id, name, os, public_key, created_at)
VALUES (sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(os), sqlc.arg(public_key), sqlc.arg(created_at)::timestamptz)
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
-- The device's stored rows for the given minutes, for the rewrite-window,
-- project-count and 60-second checks and the rows returned with a "final"
-- rejection. computed_at and snapshot are null for rows stored before
-- ADR 0017.
SELECT minute, project, label, credited, cuts, flags, rules_version, updated_at,
       computed_at, snapshot
FROM credited_minutes
WHERE device_id = sqlc.arg(device_id)
  AND minute = ANY(sqlc.arg(minutes)::timestamptz[])
ORDER BY minute, project;

-- name: CreditedMinutesUpsert :exec
-- Writes a checked batch in one round trip. rows is a JSON array of objects
-- with minute and computed_at (RFC 3339), project, label, credited, cuts,
-- flags and snapshot. Unchanged rows (key included) are skipped so retries
-- produce no dead tuples.
INSERT INTO credited_minutes (
    device_id, user_id, minute, project, label, credited, cuts, flags, rules_version, updated_at,
    computed_at, snapshot
)
SELECT sqlc.arg(device_id)::uuid, sqlc.arg(user_id)::uuid, r.minute, r.project, r.label,
       r.credited, coalesce(r.cuts, '{}'), coalesce(r.flags, '{}'),
       sqlc.arg(rules_version)::text, sqlc.arg(updated_at)::timestamptz,
       r.computed_at, r.snapshot
FROM jsonb_to_recordset(sqlc.arg(rows)::jsonb) AS r(
    minute timestamptz, project text, label text, credited smallint, cuts text[], flags text[],
    computed_at timestamptz, snapshot bigint
)
ON CONFLICT (device_id, minute, project) DO UPDATE SET
    label         = EXCLUDED.label,
    credited      = EXCLUDED.credited,
    cuts          = EXCLUDED.cuts,
    flags         = EXCLUDED.flags,
    rules_version = EXCLUDED.rules_version,
    updated_at    = EXCLUDED.updated_at,
    computed_at   = EXCLUDED.computed_at,
    snapshot      = EXCLUDED.snapshot
WHERE (credited_minutes.label, credited_minutes.credited, credited_minutes.cuts,
       credited_minutes.flags, credited_minutes.rules_version,
       credited_minutes.computed_at, credited_minutes.snapshot)
      IS DISTINCT FROM
      (EXCLUDED.label, EXCLUDED.credited, EXCLUDED.cuts, EXCLUDED.flags, EXCLUDED.rules_version,
       EXCLUDED.computed_at, EXCLUDED.snapshot);

-- name: CreditedMinutesDelete :exec
-- Deletes the device's rows for a checked list of (minute, project) pairs:
-- projects an agent removed from a still-open minute. rows is a JSON array
-- of objects with minute (RFC 3339) and project.
DELETE FROM credited_minutes c
USING jsonb_to_recordset(sqlc.arg(rows)::jsonb) AS r(minute timestamptz, project text)
WHERE c.device_id = sqlc.arg(device_id)::uuid
  AND c.minute = r.minute
  AND c.project = r.project;

-- name: EvidenceDaysSealed :many
-- The device's stored evidence days between first and last (inclusive).
-- Minutes of those days are sealed: no new rows are accepted for them.
SELECT day FROM evidence_days
WHERE device_id = sqlc.arg(device_id)
  AND day BETWEEN sqlc.arg(first_day)::date AND sqlc.arg(last_day)::date
ORDER BY day;

-- name: EvidenceDayGet :one
SELECT * FROM evidence_days WHERE device_id = $1 AND day = $2;

-- name: EvidenceDayBefore :one
-- The device's latest stored day before day: the link a new record for day
-- must continue.
SELECT * FROM evidence_days
WHERE device_id = sqlc.arg(device_id) AND day < sqlc.arg(day)
ORDER BY day DESC LIMIT 1;

-- name: EvidenceDayAfter :one
-- The device's earliest stored day after day: a new record for day must be
-- the link it continues.
SELECT * FROM evidence_days
WHERE device_id = sqlc.arg(device_id) AND day > sqlc.arg(day)
ORDER BY day LIMIT 1;

-- name: EvidenceDayInsert :exec
INSERT INTO evidence_days (
    device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version, event_counts,
    seq_from, seq_to, buckets, time_from, time_to
) VALUES (
    sqlc.arg(device_id), sqlc.arg(day), sqlc.arg(prev_chain_hash), sqlc.arg(content_hash),
    sqlc.arg(chain_hash), sqlc.arg(signature), sqlc.arg(rules_version), sqlc.arg(event_counts),
    sqlc.arg(seq_from)::bigint, sqlc.arg(seq_to)::bigint, sqlc.arg(buckets)::integer,
    sqlc.narg(time_from)::timestamptz, sqlc.narg(time_to)::timestamptz
);

-- name: EvidenceDaysOfUserDevice :many
-- The stored evidence days, oldest first, of one of the user's devices
-- between first_day and last_day (inclusive). Empty when the device isn't
-- the user's. The submission re-check (ADR 0020) reads the signed block
-- fields from here.
SELECT e.* FROM evidence_days e
JOIN devices d ON d.id = e.device_id
WHERE e.device_id = sqlc.arg(device_id)
  AND d.user_id = sqlc.arg(user_id)
  AND e.day BETWEEN sqlc.arg(first_day)::date AND sqlc.arg(last_day)::date
ORDER BY e.day;
