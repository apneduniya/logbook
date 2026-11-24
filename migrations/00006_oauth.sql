-- +goose Up
-- +goose StatementBegin

-- OAuth apps (ADR 0021). An app is also a program: its owner and reviewers
-- are its members. client_id is the app's id. A confidential app has a
-- client secret, stored only as its SHA-256; a public app (secret_hash null)
-- must use PKCE. Deleted apps keep their row and slug, so submissions keep
-- the program's name and the slug can't be taken over.
CREATE TABLE oauth_apps (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id           uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name               text NOT NULL,
    slug               text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9-]{2,40}$'),
    redirect_uris      text[] NOT NULL CHECK (cardinality(redirect_uris) BETWEEN 1 AND 5),
    homepage_url       text NOT NULL DEFAULT '',
    public             boolean NOT NULL DEFAULT false,
    secret_hash        bytea CHECK (secret_hash IS NULL OR length(secret_hash) = 32),
    secret_rotated_at  timestamptz,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    deleted_at         timestamptz
);
CREATE INDEX oauth_apps_owner_idx ON oauth_apps (owner_id) WHERE deleted_at IS NULL;

-- Program members (ADR 0020 decisions): exactly one owner per app, plus reviewers.
CREATE TABLE app_members (
    app_id      uuid NOT NULL REFERENCES oauth_apps (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role        text NOT NULL CHECK (role IN ('owner', 'reviewer')),
    added_by    uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (app_id, user_id)
);
CREATE UNIQUE INDEX app_members_one_owner_idx ON app_members (app_id) WHERE role = 'owner';
CREATE INDEX app_members_user_idx ON app_members (user_id);

-- A pending consent request, created by GET /oauth/authorize and consumed
-- by the consent page. Only the SHA-256 of its id is stored. user_id is
-- set by the first signed-in user who opens it; nobody else can then.
CREATE TABLE oauth_requests (
    id_hash                bytea PRIMARY KEY,
    app_id                 uuid NOT NULL REFERENCES oauth_apps (id) ON DELETE CASCADE,
    user_id                uuid REFERENCES users (id) ON DELETE CASCADE,
    redirect_uri           text NOT NULL,
    scopes                 text[] NOT NULL,
    state                  text NOT NULL DEFAULT '',
    code_challenge         text NOT NULL DEFAULT '',
    code_challenge_method  text NOT NULL DEFAULT '' CHECK (code_challenge_method IN ('', 'S256')),
    created_at             timestamptz NOT NULL,
    expires_at             timestamptz NOT NULL
);
CREATE INDEX oauth_requests_expires_idx ON oauth_requests (expires_at);

-- What a user has allowed an app. One row per (user, app); revoking keeps
-- the row with revoked_at set, and a later approval reopens it.
CREATE TABLE oauth_grants (
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    app_id      uuid NOT NULL REFERENCES oauth_apps (id) ON DELETE CASCADE,
    scopes      text[] NOT NULL,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    PRIMARY KEY (user_id, app_id)
);
CREATE INDEX oauth_grants_app_idx ON oauth_grants (app_id);

-- Authorization codes: single use, 10 minutes, bound to the app, user,
-- redirect URI, scopes and PKCE challenge. family_id is shared by every
-- token issued from the code and its refreshes, so a replayed code or
-- refresh token revokes all of them (RFC 6749 §4.1.2, §10.4).
CREATE TABLE oauth_codes (
    code_hash              bytea PRIMARY KEY,
    app_id                 uuid NOT NULL REFERENCES oauth_apps (id) ON DELETE CASCADE,
    user_id                uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    redirect_uri           text NOT NULL,
    scopes                 text[] NOT NULL,
    code_challenge         text NOT NULL DEFAULT '',
    code_challenge_method  text NOT NULL DEFAULT '' CHECK (code_challenge_method IN ('', 'S256')),
    family_id              uuid NOT NULL,
    created_at             timestamptz NOT NULL,
    expires_at             timestamptz NOT NULL,
    used_at                timestamptz
);
CREATE INDEX oauth_codes_user_app_idx ON oauth_codes (user_id, app_id);
CREATE INDEX oauth_codes_expires_idx ON oauth_codes (expires_at);

-- Access tokens, each with its refresh token, stored as SHA-256 hashes.
-- user_id is null for client-credentials tokens, which have no refresh
-- token. A refresh revokes the row it used and adds a new one.
CREATE TABLE oauth_tokens (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    access_hash         bytea NOT NULL UNIQUE CHECK (length(access_hash) = 32),
    refresh_hash        bytea UNIQUE CHECK (refresh_hash IS NULL OR length(refresh_hash) = 32),
    app_id              uuid NOT NULL REFERENCES oauth_apps (id) ON DELETE CASCADE,
    user_id             uuid REFERENCES users (id) ON DELETE CASCADE,
    scopes              text[] NOT NULL,
    family_id           uuid NOT NULL,
    created_at          timestamptz NOT NULL,
    expires_at          timestamptz NOT NULL,
    refresh_expires_at  timestamptz,
    revoked_at          timestamptz,
    CHECK ((refresh_hash IS NULL) = (refresh_expires_at IS NULL)),
    CHECK (user_id IS NOT NULL OR refresh_hash IS NULL)
);
CREATE INDEX oauth_tokens_user_app_idx ON oauth_tokens (user_id, app_id);
CREATE INDEX oauth_tokens_app_idx ON oauth_tokens (app_id);
CREATE INDEX oauth_tokens_family_idx ON oauth_tokens (family_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE oauth_tokens, oauth_codes, oauth_grants, oauth_requests, app_members, oauth_apps;
-- +goose StatementEnd
