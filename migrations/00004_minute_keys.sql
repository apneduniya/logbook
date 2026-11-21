-- +goose Up
-- +goose StatementBegin

-- The computation key of each credited minute (ADR 0017): computed_at is the
-- end T of the agent run's window and snapshot the event log's high-water
-- mark H it read. Rows stored before ADR 0017 have neither; uploads must send
-- both. A device-minute's rows always carry the same key.
ALTER TABLE credited_minutes
    ADD COLUMN computed_at timestamptz,
    ADD COLUMN snapshot    bigint,
    ADD CONSTRAINT credited_minutes_key_check CHECK (
        (computed_at IS NULL AND snapshot IS NULL)
        OR (snapshot >= 0
            AND date_trunc('minute', computed_at) = computed_at
            AND minute >= computed_at - interval '60 minutes'
            AND minute < computed_at)
    );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE credited_minutes
    DROP CONSTRAINT credited_minutes_key_check,
    DROP COLUMN snapshot,
    DROP COLUMN computed_at;
-- +goose StatementEnd
