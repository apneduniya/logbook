-- +goose Up
-- +goose StatementBegin
-- The Hackatime import fetches the most recent days first (the "recent"
-- segment, [recent_from, to_date]), then backfills [next_date, recent_from)
-- oldest first. next_date keeps its meaning: every day in
-- [from_date, next_date) is imported. Days in [recent_from, recent_next) are
-- imported too. Once the backfill reaches recent_from the two runs join, and
-- next_date, recent_from and recent_next move together from then on.
ALTER TABLE hackatime_imports
    ADD COLUMN recent_from date,
    ADD COLUMN recent_next date,
    ADD COLUMN recent_done boolean NOT NULL DEFAULT false;

-- Existing imports: whatever is left beyond the last 30 days of the range
-- becomes backfill; a finished import has nothing left in either segment.
UPDATE hackatime_imports
SET recent_from = GREATEST(next_date, to_date - 29),
    recent_next = GREATEST(next_date, to_date - 29),
    recent_done = status = 'done';

ALTER TABLE hackatime_imports
    ALTER COLUMN recent_from SET NOT NULL,
    ALTER COLUMN recent_next SET NOT NULL,
    ADD CONSTRAINT hackatime_imports_cursors_ordered
        CHECK (from_date <= next_date AND next_date <= recent_from AND recent_from <= recent_next);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- next_date is still a safe resume point for the single-cursor importer:
-- days after it are refetched, and ingest drops duplicates.
ALTER TABLE hackatime_imports
    DROP CONSTRAINT hackatime_imports_cursors_ordered,
    DROP COLUMN recent_done,
    DROP COLUMN recent_next,
    DROP COLUMN recent_from;
-- +goose StatementEnd
