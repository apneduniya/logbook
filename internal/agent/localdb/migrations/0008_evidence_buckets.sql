-- Evidence blocks as salted minute buckets under a Merkle root (ADR 0019).
--
-- A sealed block keeps its signed fields: the bucket count and the minute
-- span of its buckets (time_from and time_to, unix seconds of the first
-- and last minute bucket, both NULL for a block with only its repo
-- bucket). seq_from stays the first sequence number the block holds (the
-- signed seq_from is one less). A block sealed before this migration has
-- buckets = 0 and no bucket list: it can't be re-hashed or proven.
ALTER TABLE evidence_days ADD COLUMN buckets INTEGER NOT NULL DEFAULT 0;
ALTER TABLE evidence_days ADD COLUMN time_from INTEGER;
ALTER TABLE evidence_days ADD COLUMN time_to INTEGER;

-- Such a block the server hasn't accepted can never be sent (the server
-- takes only the new record), so it is dropped and its day sealed again;
-- until then its rows count as unsealed, so retention keeps them. That
-- includes one the server refused (rejected or conflict): the new server
-- refuses an old record with "update the Logbook agent", and whatever an
-- older server refused, the block sealed again gets a fresh answer. The
-- refusal shown in status goes with it, unless a refused day remains.
-- Pre-release data only.
DELETE FROM evidence_days WHERE legacy = 0 AND state <> 'accepted';
UPDATE kv SET value = (SELECT CAST(COALESCE(MAX(seq_to), 0) AS TEXT) FROM evidence_days
    WHERE legacy = 0 AND device_id = (SELECT value FROM kv WHERE key = 'crediting.device'))
WHERE key = 'log.sealed';
UPDATE kv SET value = '' WHERE key IN ('evidence.state', 'evidence.error')
    AND NOT EXISTS (SELECT 1 FROM evidence_days WHERE state IN ('rejected', 'conflict'));

-- Every sealed block's bucket list (ADR 0019 point 3), one row per leaf in
-- key order, kept for as long as the chain exists, so hidden buckets can be
-- proven after their rows are pruned. Rows of the block being sealed are
-- added here as the hasher closes them; the block's evidence_days row is
-- written when the last one is.
CREATE TABLE evidence_buckets (
    device_id   TEXT    NOT NULL,
    day         TEXT    NOT NULL,
    i           INTEGER NOT NULL, -- 0-based leaf index
    key         INTEGER NOT NULL, -- 0 for the repo bucket, else 1 + unix minute
    salt        BLOB    NOT NULL, -- 32 bytes
    bucket_hash BLOB    NOT NULL, -- 32 bytes
    PRIMARY KEY (device_id, day, i)
) WITHOUT ROWID;

-- The block being sealed, at most one (ADR 0019 point 4). It is written in
-- the transaction that freezes the block's upper bound (seq_to, the log's
-- high-water mark then) and logs the repo snapshot that opens the next
-- range. From then on no minute of the day or earlier is computed or
-- stored. The block holds the rows with seq_from < seq <= seq_to. Hashing
-- reads them by bucket key, from cursor on (0: the repo bucket is next; a
-- minute key: the first minute not read yet), and saves the hasher's state
-- with each step. Deleted when the block's evidence_days row is written,
-- or when a new device starts its chain.
CREATE TABLE evidence_sealing (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    device_id       TEXT    NOT NULL,
    day             TEXT    NOT NULL,
    prev_chain_hash TEXT    NOT NULL,
    seq_from        INTEGER NOT NULL,
    seq_to          INTEGER NOT NULL,
    cursor          INTEGER NOT NULL DEFAULT 0,
    hasher          BLOB,             -- NULL before the first step
    buckets         INTEGER NOT NULL DEFAULT 0, -- evidence_buckets rows written so far
    reserved_at     INTEGER NOT NULL  -- unix ms
);

-- Where each device's chain starts: its first block's seq_from. A device
-- that replaces another (ADR 0017 point 5) starts at the log's high-water
-- mark when it took over; a device with no row starts at 0.
CREATE TABLE evidence_baselines (
    device_id TEXT    PRIMARY KEY,
    seq_from  INTEGER NOT NULL
) WITHOUT ROWID;

-- Sealing reads a block's rows a time step at a time (ADR 0019 point 4):
-- the time indexes carry seq, so a step checks the block's sequence range
-- inside the index instead of walking the whole range in every step.
DROP INDEX heartbeats_time;
CREATE INDEX heartbeats_time ON heartbeats (time, seq);
DROP INDEX ai_events_time;
CREATE INDEX ai_events_time ON ai_events (time, seq);
DROP INDEX activity_samples_time;
CREATE INDEX activity_samples_time ON activity_samples (time, seq);
DROP INDEX git_events_time;
CREATE INDEX git_events_time ON git_events (time, seq);
DROP INDEX active_time_to;
CREATE INDEX active_time_to ON active_time (to_ms, seq);
