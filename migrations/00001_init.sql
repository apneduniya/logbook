-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS citext;

-- A Logbook account. Identity comes from Hack Club Auth (ADR 0004).
CREATE TABLE users (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hca_id          text NOT NULL UNIQUE,
    username        citext NOT NULL UNIQUE CHECK (username ~ '^[a-zA-Z0-9_-]{2,39}$'),
    display_name    text NOT NULL DEFAULT '',
    email           text NOT NULL,
    avatar_url      text NOT NULL DEFAULT '',
    timezone        text NOT NULL DEFAULT 'UTC',
    profile_public  boolean NOT NULL DEFAULT true,
    role            text NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Browser sessions. Only a SHA-256 hash of the cookie value is stored.
CREATE TABLE sessions (
    id_hash       bytea PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- API keys used by editor plugins and the agent. Only a SHA-256 hash is stored.
CREATE TABLE api_keys (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          text NOT NULL,
    key_hash      bytea NOT NULL UNIQUE,
    key_prefix    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    revoked_at    timestamptz
);
CREATE INDEX api_keys_user_id_idx ON api_keys (user_id);

-- Link to the user's Hackatime account (ADR 0004). Token is AES-GCM encrypted.
CREATE TABLE hackatime_links (
    user_id             uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    hackatime_user_id   bigint NOT NULL UNIQUE,
    hackatime_username  text NOT NULL DEFAULT '',
    access_token_enc    bytea NOT NULL,
    trust_level         text NOT NULL DEFAULT 'unknown',
    trust_checked_at    timestamptz,
    linked_at           timestamptz NOT NULL DEFAULT now()
);

-- Resumable history import from Hackatime, one row per user.
CREATE TABLE hackatime_imports (
    user_id             uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'running', 'done', 'failed')),
    from_date           date NOT NULL,
    to_date             date NOT NULL,
    next_date           date NOT NULL,
    heartbeats_imported bigint NOT NULL DEFAULT 0,
    last_error          text NOT NULL DEFAULT '',
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        text NOT NULL,
    repo_url    text NOT NULL DEFAULT '',
    hidden      boolean NOT NULL DEFAULT false,
    archived    boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

-- Raw heartbeats in WakaTime's shape, partitioned by month.
-- Dashboards never read this table; they read user_minutes.
CREATE TABLE heartbeats (
    id                  bigint GENERATED ALWAYS AS IDENTITY,
    user_id             uuid NOT NULL,
    time                timestamptz NOT NULL,
    fields_hash         bytea NOT NULL,
    source              text NOT NULL CHECK (source IN ('editor', 'agent', 'import')),
    entity              text NOT NULL,
    type                text NOT NULL DEFAULT 'file',
    category            text NOT NULL DEFAULT 'coding',
    project             text NOT NULL DEFAULT '',
    branch              text NOT NULL DEFAULT '',
    language            text NOT NULL DEFAULT '',
    editor              text NOT NULL DEFAULT '',
    plugin              text NOT NULL DEFAULT '',
    machine             text NOT NULL DEFAULT '',
    operating_system    text NOT NULL DEFAULT '',
    user_agent          text NOT NULL DEFAULT '',
    is_write            boolean NOT NULL DEFAULT false,
    lines               integer,
    lineno              integer,
    cursorpos           integer,
    line_additions      integer,
    line_deletions      integer,
    project_root_count  integer,
    dependencies        text[] NOT NULL DEFAULT '{}',
    ai_model            text,
    ai_session          text,
    ai_input_tokens     bigint,
    ai_output_tokens    bigint,
    ai_prompt_length    integer,
    ai_line_changes     integer,
    human_line_changes  integer,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, time, id),
    UNIQUE (user_id, fields_hash, time)
) PARTITION BY RANGE (time);

CREATE TABLE heartbeats_default PARTITION OF heartbeats DEFAULT;

DO $$
DECLARE
    m date := date '2024-01-01';
BEGIN
    WHILE m < date '2028-01-01' LOOP
        EXECUTE format(
            'CREATE TABLE heartbeats_%s PARTITION OF heartbeats FOR VALUES FROM (%L) TO (%L)',
            to_char(m, 'YYYYMM'), m, m + interval '1 month');
        m := m + interval '1 month';
    END LOOP;
END $$;

-- Per-minute rollup: seconds credited to (user, minute, project, language, editor, category, label).
-- label is 'you' / 'agent' / 'idle' once the struggle engine runs (ADR 0006);
-- plain editor time is 'you' until then.
CREATE TABLE user_minutes (
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    minute    timestamptz NOT NULL CHECK (date_trunc('minute', minute) = minute),
    project   text NOT NULL,
    language  text NOT NULL,
    editor    text NOT NULL,
    category  text NOT NULL,
    label     text NOT NULL DEFAULT 'you' CHECK (label IN ('you', 'agent', 'idle')),
    seconds   smallint NOT NULL CHECK (seconds BETWEEN 0 AND 60),
    PRIMARY KEY (user_id, minute, project, language, editor, category, label)
);
CREATE INDEX user_minutes_user_project_minute_idx ON user_minutes (user_id, project, minute);

-- Append-only audit trail for security-relevant and review-relevant actions.
CREATE TABLE audit_log (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    action        text NOT NULL,
    subject_type  text NOT NULL,
    subject_id    text NOT NULL,
    data          jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_subject_idx ON audit_log (subject_type, subject_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE audit_log, user_minutes, heartbeats, projects, hackatime_imports,
    hackatime_links, api_keys, sessions, users;
-- +goose StatementEnd
