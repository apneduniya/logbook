-- +goose Up
-- +goose StatementBegin

-- An agent install with its own ed25519 key (ADR 0010). Devices are revoked,
-- never deleted, so their credited minutes and evidence stay auditable.
CREATE TABLE devices (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    os            text NOT NULL CHECK (char_length(os) BETWEEN 1 AND 32),
    public_key    bytea NOT NULL UNIQUE CHECK (octet_length(public_key) = 32),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz,
    revoked_at    timestamptz,
    -- Target of the composite foreign key below, so a credited minute's
    -- user_id always matches its device's owner.
    UNIQUE (id, user_id)
);
CREATE INDEX devices_user_id_idx ON devices (user_id);

-- The agent's labelled minutes (ADR 0010, docs/STRUGGLE-RULES.md), one row
-- per (device, minute, project). Not partitioned (ADR 0014): there is at most
-- one row per device-minute-project, evidence must stay auditable for good
-- (so old partitions would never be dropped), and every read is an index
-- range scan on (user_id, minute) or (user_id, project, minute).
CREATE TABLE credited_minutes (
    device_id      uuid NOT NULL,
    user_id        uuid NOT NULL,
    minute         timestamptz NOT NULL CHECK (date_trunc('minute', minute) = minute),
    project        text NOT NULL CHECK (octet_length(project) <= 256),
    label          text NOT NULL CHECK (label IN ('you', 'agent', 'idle')),
    credited       smallint NOT NULL CHECK (credited BETWEEN 0 AND 60),
    cuts           text[] NOT NULL DEFAULT '{}',
    flags          text[] NOT NULL DEFAULT '{}',
    rules_version  text NOT NULL CHECK (char_length(rules_version) BETWEEN 1 AND 32),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (device_id, minute, project),
    FOREIGN KEY (device_id, user_id) REFERENCES devices (id, user_id) ON DELETE CASCADE,
    -- Only minutes labelled 'you' earn credit.
    CHECK (label = 'you' OR credited = 0)
);
CREATE INDEX credited_minutes_user_minute_idx ON credited_minutes (user_id, minute);
CREATE INDEX credited_minutes_user_project_minute_idx ON credited_minutes (user_id, project, minute);

-- One signed link of a device's evidence chain per UTC day (ADR 0010).
-- Hashes are lowercase hex, as defined by pkg/evidence.
CREATE TABLE evidence_days (
    device_id        uuid NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    day              date NOT NULL,
    prev_chain_hash  text NOT NULL CHECK (prev_chain_hash ~ '^([0-9a-f]{64})?$'),
    content_hash     text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    chain_hash       text NOT NULL CHECK (chain_hash ~ '^[0-9a-f]{64}$'),
    signature        bytea NOT NULL CHECK (octet_length(signature) = 64),
    rules_version    text NOT NULL CHECK (char_length(rules_version) BETWEEN 1 AND 32),
    event_counts     jsonb NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (device_id, day)
);

-- Credited seconds per (user, minute, project) across all of a user's
-- devices. When devices together credit more than 60 s in one minute, the
-- minute is capped at 60 s and each project gets its share, rounded down
-- (ADR 0014). Filters on user_id and minute are pushed into the subquery
-- (they are its window partition columns); a filter on project is applied
-- after the cap, so the cap always sees every project of the minute.
CREATE VIEW credited_project_minutes AS
SELECT t.user_id, t.minute, t.project,
       (CASE WHEN t.minute_total <= 60 THEN t.credited
             ELSE t.credited * 60 / t.minute_total END)::bigint AS seconds
FROM (
    SELECT user_id, minute, project,
           sum(credited)::bigint AS credited,
           (sum(sum(credited)) OVER (PARTITION BY user_id, minute))::bigint AS minute_total
    FROM credited_minutes
    GROUP BY user_id, minute, project
) t;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW credited_project_minutes;
DROP TABLE evidence_days, credited_minutes, devices;
-- +goose StatementEnd
