-- The Claude Code active-time records a sealed day's content hash covers,
-- exactly as hashed: a JSON array of pkg/evidence ActiveTime values, with
-- the derived fields (the session's working directory, which sessions the
-- receiver's uptime covered) as they were at sealing time. Active time has
-- no stable row range to pin (points arrive late and the derived fields are
-- read from other tables), so the records themselves are kept. NULL for days
-- sealed before this migration, and once the day's raw events are pruned.
ALTER TABLE evidence_days ADD COLUMN active_time TEXT;
