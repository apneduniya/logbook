-- Arrival time and settled finality (ADR 0016).
--
-- arrived_at is the agent's wall clock (unix ms) when it stored a raw event,
-- taken inside the inserting transaction. A minute's final credit counts
-- only events that arrived by that minute's cutoff, and a replay leaves out
-- the same late ones. Rows stored before this migration have no recorded
-- arrival: they get their own event time (to_ms for active time), the
-- earliest they can have arrived. Pre-release databases only; no day sealed
-- before this migration can be re-hashed (evidence_days.cutoffs is NULL).
ALTER TABLE heartbeats ADD COLUMN arrived_at INTEGER NOT NULL DEFAULT 0;
UPDATE heartbeats SET arrived_at = time;
ALTER TABLE ai_events ADD COLUMN arrived_at INTEGER NOT NULL DEFAULT 0;
UPDATE ai_events SET arrived_at = time;
ALTER TABLE activity_samples ADD COLUMN arrived_at INTEGER NOT NULL DEFAULT 0;
UPDATE activity_samples SET arrived_at = time;
ALTER TABLE git_events ADD COLUMN arrived_at INTEGER NOT NULL DEFAULT 0;
UPDATE git_events SET arrived_at = time;
ALTER TABLE active_time ADD COLUMN arrived_at INTEGER NOT NULL DEFAULT 0;
UPDATE active_time SET arrived_at = to_ms;

-- The arrival cutoff (unix seconds, a whole minute) of the engine run that
-- produced the row: the run read events that had arrived by then, and the
-- run's window ended there. All rows of a minute carry the same value. Rows
-- written before this migration get minute + 29 minutes, when the old
-- pipeline last rewrote a minute.
ALTER TABLE credited_minutes ADD COLUMN cutoff INTEGER NOT NULL DEFAULT 0;
UPDATE credited_minutes SET cutoff = minute + 1740;

-- Final minutes whose cutoff is not the usual minute + 29 minutes: those
-- finalized late (after a restart, a sleep or a skipped run) or never
-- computed (cutoff 0: the minute was outside every window the agent ran).
-- Ranges [from_minute, to_minute) of unix seconds; never overlapping.
CREATE TABLE credit_cutoffs (
    from_minute INTEGER PRIMARY KEY,
    to_minute   INTEGER NOT NULL,
    cutoff      INTEGER NOT NULL -- unix seconds; 0 = never computed
) WITHOUT ROWID;

-- The cutoffs of a sealed day's minutes that differ from the default, as a
-- JSON array of {"from", "to", "cutoff"}, so a replay of the day uses
-- exactly the cutoffs the agent used. NULL for days sealed before this
-- migration, which can't be replayed.
ALTER TABLE evidence_days ADD COLUMN cutoffs TEXT;
