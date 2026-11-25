-- +goose Up
-- +goose StatementBegin

-- A submission of one project's hours over a range of whole UTC days
-- (ADR 0020), optionally addressed to a program (an OAuth app, ADR 0021).
-- The review link's token is stored twice: its SHA-256 for lookups and an
-- AES-256-GCM copy (the server key, the submission id as associated data)
-- so the submitter's dashboard can show the link again. The packet never
-- holds the link.
--
-- parts_version counts the parts added: the re-check job writes its result
-- only while the version it read is still current, so a part added during a
-- run is never overwritten by that run's older result.
CREATE TABLE submissions (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    project              text NOT NULL CHECK (octet_length(project) BETWEEN 1 AND 256),
    from_day             date NOT NULL,
    to_day               date NOT NULL CHECK (to_day >= from_day AND to_day - from_day < 92),
    program_app_id       uuid REFERENCES oauth_apps (id) ON DELETE SET NULL,
    status               text NOT NULL DEFAULT 'verifying'
                         CHECK (status IN ('verifying', 'ready', 'failed', 'withdrawn')),
    error                text NOT NULL DEFAULT '',
    token_hash           bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    token_enc            bytea NOT NULL,
    -- The submitter's unmasked Hackatime trust level when it was made;
    -- null when Hackatime wasn't linked.
    trust_at_submission  text CHECK (trust_at_submission IN ('unknown', 'blue', 'red', 'green', 'yellow')),
    -- The Hackatime account linked when it was made (hackatime_links
    -- .hackatime_user_id); null when Hackatime wasn't linked. Approving
    -- needs the submitter still linked to this same account.
    hackatime_user_id    bigint,
    packet               jsonb,
    verified_minutes     integer NOT NULL DEFAULT 0,
    mismatch_minutes     integer NOT NULL DEFAULT 0,
    unbacked_minutes     integer NOT NULL DEFAULT 0,
    counted_seconds      bigint NOT NULL DEFAULT 0,
    raw_seconds          bigint NOT NULL DEFAULT 0,
    parts_version        integer NOT NULL DEFAULT 1,
    created_at           timestamptz NOT NULL,
    verified_at          timestamptz,
    decided_at           timestamptz,
    withdrawn_at         timestamptz,
    CHECK ((status = 'withdrawn') = (withdrawn_at IS NOT NULL)),
    CHECK (status <> 'withdrawn' OR decided_at IS NULL),
    CHECK ((hackatime_user_id IS NULL) = (trust_at_submission IS NULL))
);
CREATE INDEX submissions_user_idx ON submissions (user_id, created_at DESC, id);
CREATE INDEX submissions_program_idx ON submissions (program_app_id, created_at DESC, id)
    WHERE program_app_id IS NOT NULL;

-- The credited-minute rows of every device of the user for the project and
-- range, frozen when the submission was made (ADR 0020), with what the
-- re-check found: result is null while verifying; server_* is the replay's
-- row on a mismatch. The project is the submission's.
CREATE TABLE submission_minutes (
    submission_id    uuid NOT NULL REFERENCES submissions (id) ON DELETE CASCADE,
    device_id        uuid NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    minute           timestamptz NOT NULL,
    label            text NOT NULL,
    credited         smallint NOT NULL,
    cuts             text[] NOT NULL,
    flags            text[] NOT NULL,
    rules_version    text NOT NULL,
    computed_at      timestamptz,
    snapshot         bigint,
    result           text CHECK (result IN ('verified', 'mismatch', 'unbacked')),
    reason           text NOT NULL DEFAULT '',
    server_label     text,
    server_credited  smallint,
    server_cuts      text[],
    PRIMARY KEY (submission_id, device_id, minute),
    CHECK ((result = 'mismatch') = (server_label IS NOT NULL))
);

-- The project's capped seconds (credited_project_minutes, ADR 0014) for
-- each minute of the frozen rows, 0 included, from the same snapshot.
CREATE TABLE submission_capped (
    submission_id  uuid NOT NULL REFERENCES submissions (id) ON DELETE CASCADE,
    minute         timestamptz NOT NULL,
    seconds        bigint NOT NULL CHECK (seconds BETWEEN 0 AND 60),
    PRIMARY KEY (submission_id, minute)
);

-- Every credited_minutes row of the user (any project, any device, whatever
-- its credit) at the minutes of the frozen rows, from the same snapshot, so
-- the cap can be recomputed offline (logbook verify).
CREATE TABLE submission_cap_inputs (
    submission_id  uuid NOT NULL REFERENCES submissions (id) ON DELETE CASCADE,
    device_id      uuid NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    minute         timestamptz NOT NULL,
    project        text NOT NULL,
    credited       smallint NOT NULL,
    PRIMARY KEY (submission_id, device_id, minute, project)
);

-- One device's evidence part (submission.Part), gzip-compressed JSON. data
-- is set to null when the raw rows are deleted (withdrawal, or one year
-- after the latest decision); bytes is the compressed size.
CREATE TABLE submission_parts (
    submission_id   uuid NOT NULL REFERENCES submissions (id) ON DELETE CASCADE,
    device_id       uuid NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    data            bytea,
    bytes           integer NOT NULL CHECK (bytes >= 0),
    uploaded_at     timestamptz NOT NULL,
    raw_deleted_at  timestamptz,
    PRIMARY KEY (submission_id, device_id),
    CHECK ((data IS NULL) = (raw_deleted_at IS NOT NULL))
);
CREATE INDEX submission_parts_raw_idx ON submission_parts (submission_id) WHERE raw_deleted_at IS NULL;

-- Every decision a program member recorded; the newest wins.
CREATE TABLE submission_decisions (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    submission_id     uuid NOT NULL REFERENCES submissions (id) ON DELETE CASCADE,
    value             text NOT NULL CHECK (value IN ('approved', 'adjusted', 'rejected')),
    approved_seconds  bigint CHECK (approved_seconds >= 0),
    reason            text NOT NULL DEFAULT '' CHECK (char_length(reason) <= 500),
    decided_by        uuid REFERENCES users (id) ON DELETE SET NULL,
    decided_at        timestamptz NOT NULL,
    CHECK ((value = 'adjusted') = (approved_seconds IS NOT NULL)),
    CHECK (value = 'approved' OR reason <> '')
);
CREATE INDEX submission_decisions_submission_idx ON submission_decisions (submission_id, decided_at DESC, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE submission_decisions, submission_parts, submission_cap_inputs, submission_capped,
    submission_minutes, submissions;
-- +goose StatementEnd
