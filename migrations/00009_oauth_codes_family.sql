-- +goose Up
-- +goose StatementBegin

-- OAuth cleanup deletes dead codes one token family at a time, under that
-- family's lock (internal/server/oauth/cleanup.go), so it looks codes up
-- by family.
CREATE INDEX oauth_codes_family_idx ON oauth_codes (family_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX oauth_codes_family_idx;
-- +goose StatementEnd
