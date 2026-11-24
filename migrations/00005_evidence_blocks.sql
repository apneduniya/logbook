-- +goose Up
-- +goose StatementBegin

-- The signed block fields of an evidence day (ADR 0019): the block holds the
-- device's logged rows with seq_from < seq <= seq_to, it has `buckets`
-- leaves, and its minute buckets span [time_from, time_to] (both null for a
-- block with only its repo bucket). Rows stored before ADR 0019 (dev only)
-- have none of them; the server requires them on every new upload. The
-- check matches evidence.Day.Validate. Each branch tests IS NOT NULL first,
-- because a CHECK that evaluates to null passes.
ALTER TABLE evidence_days
    ADD COLUMN seq_from  bigint,
    ADD COLUMN seq_to    bigint,
    ADD COLUMN buckets   integer,
    ADD COLUMN time_from timestamptz,
    ADD COLUMN time_to   timestamptz,
    ADD CONSTRAINT evidence_days_block_check CHECK (
        (seq_from IS NULL AND seq_to IS NULL AND buckets IS NULL AND time_from IS NULL AND time_to IS NULL)
        OR (seq_from IS NOT NULL AND seq_to IS NOT NULL AND buckets IS NOT NULL
            AND seq_from >= 0 AND seq_to > seq_from AND buckets >= 1
            AND ((time_from IS NULL AND time_to IS NULL AND buckets = 1)
                 OR (time_from IS NOT NULL AND time_to IS NOT NULL
                     AND date_trunc('minute', time_from) = time_from
                     AND date_trunc('minute', time_to) = time_to
                     AND time_from >= '1970-01-01T00:00:00Z'
                     AND time_to >= time_from
                     AND buckets >= 2
                     AND buckets - 1 <= extract(epoch FROM time_to - time_from) / 60 + 1)))
    );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE evidence_days
    DROP CONSTRAINT evidence_days_block_check,
    DROP COLUMN time_to,
    DROP COLUMN time_from,
    DROP COLUMN buckets,
    DROP COLUMN seq_to,
    DROP COLUMN seq_from;
-- +goose StatementEnd
