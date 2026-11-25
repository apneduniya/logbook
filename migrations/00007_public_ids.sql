-- +goose Up
-- +goose StatementBegin

-- Numeric user ids for the Hackatime-shaped program API (ADR 0021). Hackatime
-- ids are integers and typed clients decode them as such, so every
-- Hackatime-shaped response puts public_id in "id". Existing rows are
-- numbered when the column is added. Ids are never reused.
--
-- They start at 100000001 so a Hackatime user id that a program stored before
-- switching to Logbook (Hackatime's ids are far smaller) never names a
-- different Logbook user.
ALTER TABLE users
    ADD COLUMN public_id bigint NOT NULL GENERATED ALWAYS AS IDENTITY (START WITH 100000001) UNIQUE;

-- "my" is the token's own user on the program API's stats endpoints
-- (/api/v1/users/my/stats), so it can't be anyone's username. Nobody should
-- hold it yet; if somebody does, they are renamed (and it is audited) rather
-- than the migration failing.
INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id, data)
SELECT NULL, 'user.username_reserved', 'user', id::text,
       jsonb_build_object('from', username::text, 'to', 'my-user-' || public_id)
FROM users WHERE username = 'my';
UPDATE users SET username = 'my-user-' || public_id WHERE username = 'my';

ALTER TABLE users ADD CONSTRAINT users_username_not_reserved CHECK (username <> 'my');

-- Every Hackatime trust level Logbook read for a user, newest checked_at
-- last. One row per stretch of an unchanged level: a refresh that reads the
-- same level as the newest row moves that row's checked_at forward, and a
-- different level starts a new row. Kept after unlinking (the admin API
-- reads the newest 25, ADR 0022).
CREATE TABLE hackatime_trust_history (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    level       text NOT NULL CHECK (level IN ('unknown', 'blue', 'red', 'green', 'yellow')),
    checked_at  timestamptz NOT NULL
);
CREATE INDEX hackatime_trust_history_user_idx ON hackatime_trust_history (user_id, checked_at DESC, id DESC);

-- Seed the history with the level each existing link holds now.
INSERT INTO hackatime_trust_history (user_id, level, checked_at)
SELECT user_id, trust_level, coalesce(trust_checked_at, linked_at)
FROM hackatime_links;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE hackatime_trust_history;
ALTER TABLE users DROP CONSTRAINT users_username_not_reserved;
ALTER TABLE users DROP COLUMN public_id;
-- +goose StatementEnd
