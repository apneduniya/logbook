-- Full editor heartbeats, kept locally for 90 days so the struggle engine and
-- the daily evidence hash can read them (the upload queues delete heartbeats
-- once sent, and heartbeat_log keeps only time, project and directory for two
-- days). payload is the wire.Heartbeat JSON exactly as accepted. A heartbeat
-- that wakatime-cli resends is stored once (fields_hash is wire.FieldsHash).
CREATE TABLE heartbeats (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    time        INTEGER NOT NULL, -- unix ms of the heartbeat time
    fields_hash BLOB    NOT NULL,
    payload     BLOB    NOT NULL
);
CREATE INDEX heartbeats_time ON heartbeats (time);
CREATE UNIQUE INDEX heartbeats_fields ON heartbeats (fields_hash);

-- The struggle engine's output per (minute, project), and what the server has
-- accepted of it. digest identifies the row's content; sent_digest is the
-- digest the server last accepted (or rejected for good); sent_final is 1 once
-- the minute is final and nothing more will be sent for it.
CREATE TABLE credited_minutes (
    minute        INTEGER NOT NULL, -- unix seconds, UTC, whole minute
    project       TEXT    NOT NULL,
    label         TEXT    NOT NULL,
    credited      INTEGER NOT NULL,
    cuts          TEXT    NOT NULL, -- JSON array of cut codes
    flags         TEXT    NOT NULL, -- JSON array of flag codes
    rules_version TEXT    NOT NULL,
    digest        TEXT    NOT NULL,
    sent_digest   TEXT    NOT NULL DEFAULT '',
    sent_final    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (minute, project)
) WITHOUT ROWID;
CREATE INDEX credited_minutes_unsent ON credited_minutes (sent_final, minute);

-- One link of a device's evidence chain per UTC day. A row is written
-- (state 'pending') when the day is sealed and signed, so retries send the
-- exact same values; it becomes 'accepted' when the server stores it, and
-- 'conflict' or 'rejected' when the server refuses it, which stops the chain.
-- hw_* are the highest row ids of each event table at sealing time: the
-- day's content is exactly its events with ids at or below them, so a bundle
-- can reproduce the hash even after late events arrive.
CREATE TABLE evidence_days (
    device_id       TEXT    NOT NULL,
    day             TEXT    NOT NULL, -- YYYY-MM-DD (UTC)
    prev_chain_hash TEXT    NOT NULL,
    content_hash    TEXT    NOT NULL,
    chain_hash      TEXT    NOT NULL,
    signature       BLOB    NOT NULL,
    rules_version   TEXT    NOT NULL,
    event_counts    TEXT    NOT NULL, -- JSON object
    hw_heartbeats   INTEGER NOT NULL,
    hw_ai           INTEGER NOT NULL,
    hw_activity     INTEGER NOT NULL,
    hw_git          INTEGER NOT NULL,
    state           TEXT    NOT NULL,
    error           TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL, -- unix ms
    updated_at      INTEGER NOT NULL, -- unix ms
    PRIMARY KEY (device_id, day)
) WITHOUT ROWID;
