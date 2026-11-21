-- One append-only event log and recorded finality (ADR 0017).
--
-- Every row the struggle engine can read (heartbeats, AI events, activity
-- samples, git events, Claude Code active time) and every repo mapping row
-- gets a sequence number from one counter, kv 'log.seq', taken inside the
-- inserting transaction. SQLite serialises writers, so the committed rows
-- are exactly the sequence numbers 1..H that are still present. Rows stored
-- before this migration are numbered in the order they arrived: by
-- arrived_at (since for repo rows), then table name, then row id.
ALTER TABLE heartbeats ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ai_events ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE activity_samples ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE git_events ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE active_time ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE repo_map ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;

CREATE TEMP TABLE log_backfill AS
SELECT tbl, id, ROW_NUMBER() OVER (ORDER BY arrived, tbl, id) AS seq FROM (
    SELECT 'active_time' AS tbl, id, arrived_at AS arrived FROM active_time
    UNION ALL SELECT 'activity_samples', id, arrived_at FROM activity_samples
    UNION ALL SELECT 'ai_events', id, arrived_at FROM ai_events
    UNION ALL SELECT 'git_events', id, arrived_at FROM git_events
    UNION ALL SELECT 'heartbeats', id, arrived_at FROM heartbeats
    UNION ALL SELECT 'repo_map', id, since FROM repo_map
);
CREATE INDEX temp.log_backfill_row ON log_backfill (tbl, id);
UPDATE heartbeats SET seq = (SELECT seq FROM log_backfill b WHERE b.tbl = 'heartbeats' AND b.id = heartbeats.id);
UPDATE ai_events SET seq = (SELECT seq FROM log_backfill b WHERE b.tbl = 'ai_events' AND b.id = ai_events.id);
UPDATE activity_samples SET seq = (SELECT seq FROM log_backfill b WHERE b.tbl = 'activity_samples' AND b.id = activity_samples.id);
UPDATE git_events SET seq = (SELECT seq FROM log_backfill b WHERE b.tbl = 'git_events' AND b.id = git_events.id);
UPDATE active_time SET seq = (SELECT seq FROM log_backfill b WHERE b.tbl = 'active_time' AND b.id = active_time.id);
UPDATE repo_map SET seq = (SELECT seq FROM log_backfill b WHERE b.tbl = 'repo_map' AND b.id = repo_map.id);
INSERT INTO kv (key, value, updated_at)
SELECT 'log.seq', CAST(COUNT(*) AS TEXT), 0 FROM log_backfill;
DROP TABLE log_backfill;

CREATE UNIQUE INDEX heartbeats_seq ON heartbeats (seq);
CREATE UNIQUE INDEX ai_events_seq ON ai_events (seq);
CREATE UNIQUE INDEX activity_samples_seq ON activity_samples (seq);
CREATE UNIQUE INDEX git_events_seq ON git_events (seq);
CREATE UNIQUE INDEX active_time_seq ON active_time (seq);
CREATE UNIQUE INDEX repo_map_seq ON repo_map (seq);

ALTER TABLE heartbeats DROP COLUMN arrived_at;
ALTER TABLE ai_events DROP COLUMN arrived_at;
ALTER TABLE activity_samples DROP COLUMN arrived_at;
ALTER TABLE git_events DROP COLUMN arrived_at;
ALTER TABLE active_time DROP COLUMN arrived_at;

-- Credited minutes record the computation key that produced them,
-- computed_at (T, unix seconds) and snapshot (H, the log's high-water
-- mark), and whether the minute is open or final. A device-minute is one
-- unit: all of its rows carry the same key and state. acknowledged is 1
-- once the server has answered for the row as it now is (accepted it, sent
-- back its own rows, or refused it); sent_digest becomes what the server
-- last accepted for (minute, project), digest and key ('' if none). A row with no
-- key (both NULL) was stored before this migration or adopted from a server
-- row stored before it; it is never recomputed.
--
-- Rows from before this migration become final, acknowledged and unkeyed
-- when the server had answered for them; rows it hadn't answered for are
-- dropped (they can't be uploaded without a key). Pre-release data only.
DELETE FROM credited_minutes WHERE sent_final = 0 AND digest <> sent_digest;
DROP INDEX credited_minutes_unsent;
ALTER TABLE credited_minutes ADD COLUMN computed_at INTEGER;
ALTER TABLE credited_minutes ADD COLUMN snapshot INTEGER;
ALTER TABLE credited_minutes ADD COLUMN state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'final'));
ALTER TABLE credited_minutes ADD COLUMN acknowledged INTEGER NOT NULL DEFAULT 0;
UPDATE credited_minutes SET state = 'final', acknowledged = 1;
ALTER TABLE credited_minutes DROP COLUMN cutoff;
ALTER TABLE credited_minutes DROP COLUMN sent_final;
CREATE INDEX credited_minutes_unacknowledged ON credited_minutes (acknowledged, minute);

-- Final minutes, as merged ranges [from_minute, to_minute) of unix seconds,
-- never overlapping. A final minute is never recomputed, whatever the clock
-- does later; this also covers final minutes with no rows. Everything the
-- old watermark (or a stored row) made final stays final.
CREATE TABLE final_spans (
    from_minute INTEGER PRIMARY KEY,
    to_minute   INTEGER NOT NULL
) WITHOUT ROWID;
INSERT INTO final_spans (from_minute, to_minute)
SELECT 0, t FROM (SELECT MAX(t) AS t FROM (
    SELECT CAST(strftime('%s', value) AS INTEGER) AS t FROM kv WHERE key = 'crediting.final_through'
    UNION ALL SELECT MAX(minute) + 60 FROM credited_minutes
)) WHERE t IS NOT NULL;
DROP TABLE credit_cutoffs;
DELETE FROM kv WHERE key IN ('crediting.final_through', 'crediting.late_mark');

-- An evidence block seals the log rows with seq in [seq_from, seq_to].
-- Days sealed before this migration covered rows by event time and can't
-- be replayed: they are marked legacy, and the device's next block starts
-- again at the oldest retained row.
ALTER TABLE evidence_days ADD COLUMN seq_from INTEGER NOT NULL DEFAULT 0;
ALTER TABLE evidence_days ADD COLUMN seq_to INTEGER NOT NULL DEFAULT 0;
ALTER TABLE evidence_days ADD COLUMN legacy INTEGER NOT NULL DEFAULT 0;
UPDATE evidence_days SET legacy = 1;
ALTER TABLE evidence_days DROP COLUMN hw_heartbeats;
ALTER TABLE evidence_days DROP COLUMN hw_ai;
ALTER TABLE evidence_days DROP COLUMN hw_activity;
ALTER TABLE evidence_days DROP COLUMN hw_git;
ALTER TABLE evidence_days DROP COLUMN active_time;
ALTER TABLE evidence_days DROP COLUMN cutoffs;

-- A path's mapping at H is its row with the highest seq <= H.
DROP INDEX repo_map_path;
CREATE INDEX repo_map_path ON repo_map (path, seq);
