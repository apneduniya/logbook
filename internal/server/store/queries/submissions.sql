-- Queries for internal/server/submissions (ADR 0020). Every read of a
-- submission is scoped by its owner (user_id), its program (program_app_id),
-- or its review token's hash, which the caller has already resolved.

-- name: SubmissionPlanRows :many
-- The user's credited rows of one project with a minute in [from_time,
-- to_time), every device and label, by (device, minute).
SELECT device_id, minute, label, credited, cuts, flags, rules_version, computed_at, snapshot
FROM credited_minutes
WHERE user_id = sqlc.arg(user_id)
  AND project = sqlc.arg(project)
  AND minute >= sqlc.arg(from_time)
  AND minute < sqlc.arg(to_time)
ORDER BY device_id, minute;

-- name: SubmissionPlanDevices :many
-- The devices with credited rows of one project in [from_time, to_time):
-- the last day each has sealed (null when none), whether it is revoked,
-- and how many of its rows carry credit.
SELECT d.id, d.name, d.os,
       (SELECT max(e.day) FROM evidence_days e WHERE e.device_id = d.id)::date AS sealed_through,
       (d.revoked_at IS NOT NULL)::bool AS revoked,
       count(*) FILTER (WHERE c.credited > 0)::bigint AS credited_rows
FROM credited_minutes c
JOIN devices d ON d.id = c.device_id
WHERE c.user_id = sqlc.arg(user_id)
  AND c.project = sqlc.arg(project)
  AND c.minute >= sqlc.arg(from_time)
  AND c.minute < sqlc.arg(to_time)
GROUP BY d.id
ORDER BY d.id;

-- name: SubmissionPublicProgram :one
-- A live public app by slug: what a submission may be addressed to.
SELECT id, slug, name FROM oauth_apps
WHERE slug = sqlc.arg(slug) AND public AND deleted_at IS NULL;

-- name: SubmissionPrograms :many
-- The slug and name of apps by id, deleted ones included: a submission
-- keeps its program's name.
SELECT id, slug, name FROM oauth_apps WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: SubmissionUserDevice :one
-- One of the user's devices, revoked or not.
SELECT id, name, os, public_key FROM devices
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: SubmissionBlockDays :many
-- The days among days for which the device has a stored evidence record
-- with block fields (ADR 0019), which a proof can be checked against.
SELECT to_char(day, 'YYYY-MM-DD')::text AS day FROM evidence_days
WHERE device_id = sqlc.arg(device_id)
  AND seq_to IS NOT NULL
  AND day = ANY(sqlc.arg(days)::date[]);

-- name: SubmissionInsert :exec
INSERT INTO submissions (id, user_id, project, from_day, to_day, program_app_id, token_hash, token_enc,
                         trust_at_submission, hackatime_user_id, created_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(project), sqlc.arg(from_day), sqlc.arg(to_day),
        sqlc.narg(program_app_id), sqlc.arg(token_hash), sqlc.arg(token_enc),
        sqlc.narg(trust_at_submission), sqlc.narg(hackatime_user_id), sqlc.arg(created_at)::timestamptz);

-- name: SubmissionFreezeMinutes :execrows
-- Freezes the user's rows of the submission's project and range, every
-- device and label.
INSERT INTO submission_minutes (submission_id, device_id, minute, label, credited, cuts, flags,
                                rules_version, computed_at, snapshot)
SELECT s.id, c.device_id, c.minute, c.label, c.credited, c.cuts, c.flags, c.rules_version,
       c.computed_at, c.snapshot
FROM submissions s
JOIN credited_minutes c ON c.user_id = s.user_id AND c.project = s.project
WHERE s.id = sqlc.arg(submission_id)
  AND c.minute >= s.from_day::timestamp AT TIME ZONE 'UTC'
  AND c.minute < (s.to_day + 1)::timestamp AT TIME ZONE 'UTC';

-- name: SubmissionFreezeCapInputs :execrows
-- Freezes every credited row of the user, any project and device, at the
-- minutes of the frozen rows.
INSERT INTO submission_cap_inputs (submission_id, device_id, minute, project, credited)
SELECT s.id, c.device_id, c.minute, c.project, c.credited
FROM submissions s
JOIN credited_minutes c ON c.user_id = s.user_id
WHERE s.id = sqlc.arg(submission_id)
  AND c.minute IN (SELECT DISTINCT m.minute FROM submission_minutes m WHERE m.submission_id = s.id);

-- name: SubmissionFreezeCapped :execrows
-- Freezes the project's capped seconds at the minutes of the frozen rows,
-- as credited_project_minutes gives them.
INSERT INTO submission_capped (submission_id, minute, seconds)
SELECT s.id, v.minute, v.seconds
FROM submissions s
JOIN credited_project_minutes v ON v.user_id = s.user_id AND v.project = s.project
WHERE s.id = sqlc.arg(submission_id)
  AND v.minute >= s.from_day::timestamp AT TIME ZONE 'UTC'
  AND v.minute < (s.to_day + 1)::timestamp AT TIME ZONE 'UTC';

-- name: SubmissionPartInsert :execrows
-- Stores a device's part. No row is written when the device already has one.
INSERT INTO submission_parts (submission_id, device_id, data, bytes, uploaded_at)
VALUES (sqlc.arg(submission_id), sqlc.arg(device_id), sqlc.arg(data), sqlc.arg(bytes),
        sqlc.arg(uploaded_at)::timestamptz)
ON CONFLICT (submission_id, device_id) DO NOTHING;

-- name: SubmissionLockOfUser :one
-- Locks one of the user's submissions until the transaction ends.
SELECT * FROM submissions WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) FOR UPDATE;

-- name: SubmissionPartsChanged :one
-- A part was added: the whole submission is checked again.
UPDATE submissions SET status = 'verifying', error = '', parts_version = parts_version + 1
WHERE id = sqlc.arg(id)
RETURNING parts_version;

-- name: SubmissionGet :one
-- One submission by id, for the re-check job (which got the id from a job
-- the server queued) and for callers that have already checked access.
SELECT * FROM submissions WHERE id = $1;

-- name: SubmissionLock :one
SELECT * FROM submissions WHERE id = $1 FOR UPDATE;

-- name: SubmissionFrozenMinutes :many
SELECT device_id, minute, label, credited, cuts, flags, rules_version, computed_at, snapshot
FROM submission_minutes
WHERE submission_id = $1
ORDER BY device_id, minute;

-- name: SubmissionFrozenDevices :many
-- The devices with frozen rows of a submission: the last day each has
-- sealed (null when none) and whether it is revoked.
SELECT d.id, d.name,
       (SELECT max(e.day) FROM evidence_days e WHERE e.device_id = d.id)::date AS sealed_through,
       (d.revoked_at IS NOT NULL)::bool AS revoked
FROM devices d
WHERE d.id IN (SELECT DISTINCT m.device_id FROM submission_minutes m WHERE m.submission_id = $1)
ORDER BY d.id;

-- name: SubmissionFrozenMinutesOfDevice :many
-- One device's frozen rows of a submission.
SELECT device_id, minute, label, credited, cuts, flags, rules_version, computed_at, snapshot
FROM submission_minutes
WHERE submission_id = sqlc.arg(submission_id) AND device_id = sqlc.arg(device_id)
ORDER BY minute;

-- name: SubmissionRawDeleted :one
-- Whether any stored part of a submission has been deleted.
SELECT EXISTS (
    SELECT 1 FROM submission_parts WHERE submission_id = $1 AND raw_deleted_at IS NOT NULL
)::bool AS deleted;

-- name: SubmissionCapped :many
SELECT minute, seconds FROM submission_capped WHERE submission_id = $1 ORDER BY minute;

-- name: SubmissionCapInputs :many
SELECT device_id, minute, project, credited FROM submission_cap_inputs
WHERE submission_id = $1
ORDER BY minute, device_id, project;

-- name: SubmissionPartDevices :many
-- The devices with a part, and whether its raw rows were deleted, without
-- the parts themselves (the re-check reads them one at a time).
SELECT device_id, (data IS NULL)::boolean AS deleted FROM submission_parts
WHERE submission_id = $1
ORDER BY device_id;

-- name: SubmissionPartData :one
-- One device's stored part; null once its raw rows were deleted.
SELECT data FROM submission_parts
WHERE submission_id = $1 AND device_id = $2;

-- name: SubmissionParts :many
SELECT device_id, data, bytes, uploaded_at, raw_deleted_at FROM submission_parts
WHERE submission_id = $1
ORDER BY device_id;

-- name: SubmissionRawDays :many
-- Time at the keyboard (user_minutes, label you, as /api/v1/me/summary
-- counts it) for one project per UTC day in [from_time, to_time), and its
-- part in category "ai coding".
SELECT to_char((minute AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD')::text AS day,
       sum(seconds)::bigint AS seconds,
       coalesce(sum(seconds) FILTER (WHERE lower(category) = 'ai coding'), 0)::bigint AS ai_seconds
FROM user_minutes
WHERE user_id = sqlc.arg(user_id)
  AND project = sqlc.arg(project)
  AND label = 'you'
  AND minute >= sqlc.arg(from_time)
  AND minute < sqlc.arg(to_time)
GROUP BY 1
ORDER BY 1;

-- name: SubmissionSetResults :exec
-- Writes the re-check's result of every frozen row. rows is a JSON array of
-- objects with device_id, minute (RFC 3339), result, reason and, on a
-- mismatch, server_label, server_credited and server_cuts.
UPDATE submission_minutes m SET
    result          = r.result,
    reason          = coalesce(r.reason, ''),
    server_label    = r.server_label,
    server_credited = r.server_credited,
    server_cuts     = r.server_cuts
FROM jsonb_to_recordset(sqlc.arg(rows)::jsonb) AS r(
    device_id uuid, minute timestamptz, result text, reason text,
    server_label text, server_credited smallint, server_cuts text[]
)
WHERE m.submission_id = sqlc.arg(submission_id)
  AND m.device_id = r.device_id
  AND m.minute = r.minute;

-- name: SubmissionSetReady :exec
UPDATE submissions SET
    status           = 'ready',
    error            = '',
    packet           = sqlc.arg(packet),
    verified_minutes = sqlc.arg(verified_minutes),
    mismatch_minutes = sqlc.arg(mismatch_minutes),
    unbacked_minutes = sqlc.arg(unbacked_minutes),
    counted_seconds  = sqlc.arg(counted_seconds),
    raw_seconds      = sqlc.arg(raw_seconds),
    verified_at      = sqlc.arg(verified_at)::timestamptz
WHERE id = sqlc.arg(id);

-- name: SubmissionSetFailed :exec
UPDATE submissions SET
    status           = 'failed',
    error            = sqlc.arg(error),
    packet           = NULL,
    verified_minutes = 0,
    mismatch_minutes = 0,
    unbacked_minutes = 0,
    counted_seconds  = 0,
    raw_seconds      = 0,
    verified_at      = sqlc.arg(verified_at)::timestamptz
WHERE id = sqlc.arg(id);

-- name: SubmissionsOfUser :many
SELECT * FROM submissions WHERE user_id = $1 ORDER BY created_at DESC, id;

-- name: SubmissionOfUser :one
SELECT * FROM submissions WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: SubmissionByTokenHash :one
SELECT * FROM submissions WHERE token_hash = $1;

-- name: SubmissionLockByTokenHash :one
SELECT * FROM submissions WHERE token_hash = $1 FOR UPDATE;

-- name: SubmissionsOfProgram :many
-- The submissions addressed to an app, newest first. A null status or
-- decision is no filter; decision 'none' matches submissions without one.
SELECT s.* FROM submissions s
LEFT JOIN LATERAL (
    SELECT d.value FROM submission_decisions d
    WHERE d.submission_id = s.id
    ORDER BY d.decided_at DESC, d.id DESC
    LIMIT 1
) latest ON true
WHERE s.program_app_id = sqlc.arg(app_id)
  AND (sqlc.narg(status)::text IS NULL OR s.status = sqlc.narg(status)::text)
  AND (sqlc.narg(decision)::text IS NULL
       OR coalesce(latest.value, 'none') = sqlc.narg(decision)::text)
ORDER BY s.created_at DESC, s.id
LIMIT sqlc.arg(max_rows) OFFSET sqlc.arg(skip_rows);

-- name: SubmissionOfProgram :one
SELECT * FROM submissions WHERE id = sqlc.arg(id) AND program_app_id = sqlc.arg(app_id);

-- name: SubmissionLatestDecisions :many
-- The newest decision of each submission, with the decider's username
-- ("" once their account is gone).
SELECT DISTINCT ON (d.submission_id)
       d.submission_id, d.value, d.approved_seconds, d.reason, d.decided_at,
       coalesce(u.username::text, '')::text AS decided_by_username
FROM submission_decisions d
LEFT JOIN users u ON u.id = d.decided_by
WHERE d.submission_id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY d.submission_id, d.decided_at DESC, d.id DESC;

-- name: SubmissionSubmitters :many
-- What reviewers see of submitters: username, display name and Hackatime
-- user id (null when not linked).
SELECT u.id, u.username::text AS username, u.display_name, h.hackatime_user_id
FROM users u
LEFT JOIN hackatime_links h ON h.user_id = u.id
WHERE u.id = ANY(sqlc.arg(ids)::uuid[]);

-- name: SubmissionDecisionInsert :one
INSERT INTO submission_decisions (submission_id, value, approved_seconds, reason, decided_by, decided_at)
VALUES (sqlc.arg(submission_id), sqlc.arg(value), sqlc.narg(approved_seconds), sqlc.arg(reason),
        sqlc.arg(decided_by), sqlc.arg(decided_at)::timestamptz)
RETURNING id;

-- name: SubmissionSetDecided :exec
UPDATE submissions SET decided_at = sqlc.arg(decided_at)::timestamptz WHERE id = sqlc.arg(id);

-- name: SubmissionWithdraw :exec
UPDATE submissions SET status = 'withdrawn', withdrawn_at = sqlc.arg(withdrawn_at)::timestamptz
WHERE id = sqlc.arg(id);

-- name: SubmissionDeleteRaw :execrows
-- Deletes the raw rows (the stored parts) of one submission.
UPDATE submission_parts SET data = NULL, raw_deleted_at = sqlc.arg(deleted_at)::timestamptz
WHERE submission_id = sqlc.arg(submission_id) AND raw_deleted_at IS NULL;

-- name: SubmissionExpireRaw :execrows
-- Deletes the raw rows of submissions whose latest decision (or, with
-- none, creation) is before cutoff.
UPDATE submission_parts p SET data = NULL, raw_deleted_at = sqlc.arg(deleted_at)::timestamptz
FROM submissions s
WHERE p.submission_id = s.id
  AND p.raw_deleted_at IS NULL
  AND coalesce(s.decided_at, s.created_at) < sqlc.arg(cutoff)::timestamptz;
