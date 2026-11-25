-- +goose Up
-- +goose StatementBegin

-- Admin API keys (ADR 0022). Made only on the server's command line for a
-- user with role 'admin', printed once, and stored only as the SHA-256 of
-- the whole key. The key is "lba_" plus 256 random bits in base64url; its
-- first 12 characters after "lba_" are kept in clear as prefix, to tell
-- keys apart and to revoke one. A key is never deleted: revoking sets
-- revoked_at, and revoking a user's admin role revokes all of theirs.
CREATE TABLE admin_keys (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    prefix        text NOT NULL UNIQUE CHECK (prefix ~ '^lba_[A-Za-z0-9_-]{12}$'),
    key_hash      bytea NOT NULL UNIQUE CHECK (length(key_hash) = 32),
    created_at    timestamptz NOT NULL,
    last_used_at  timestamptz,
    revoked_at    timestamptz
);
CREATE INDEX admin_keys_user_idx ON admin_keys (user_id);

-- The admin API's user/audit_log reads a user's entries as actor as well
-- as subject (audit_log_subject_idx), newest first.
CREATE INDEX audit_log_actor_idx ON audit_log (actor_user_id, id DESC);

-- user/get_user_by_email looks a user up by exact email.
CREATE INDEX users_email_idx ON users (email);

-- When each stretch of an unchanged Hackatime trust level was first read,
-- which user/trust_logs shows as the change's created_at. checked_at keeps
-- moving forward while the level stays the same. Rows from before this
-- migration only know their latest reading.
ALTER TABLE hackatime_trust_history ADD COLUMN first_checked_at timestamptz;
UPDATE hackatime_trust_history SET first_checked_at = checked_at;
ALTER TABLE hackatime_trust_history ALTER COLUMN first_checked_at SET NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE hackatime_trust_history DROP COLUMN first_checked_at;
DROP INDEX users_email_idx;
DROP INDEX audit_log_actor_idx;
DROP TABLE admin_keys;
-- +goose StatementEnd
