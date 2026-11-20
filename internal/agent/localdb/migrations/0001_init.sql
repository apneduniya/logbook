-- Upload queue for the Logbook server. payload is the heartbeat JSON.
CREATE TABLE heartbeat_queue (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    enqueued_at INTEGER NOT NULL, -- unix ms
    payload     BLOB    NOT NULL
);

-- Mirror queue for Hackatime, same shape.
CREATE TABLE mirror_queue (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    enqueued_at INTEGER NOT NULL,
    payload     BLOB    NOT NULL
);

-- Short local log of accepted heartbeats (two days), for the offline status
-- bar estimate, today's counts and repo discovery. Only the directory of the
-- entity is kept, never the file name.
CREATE TABLE heartbeat_log (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    time    INTEGER NOT NULL, -- unix ms
    project TEXT    NOT NULL DEFAULT '',
    dir     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX heartbeat_log_time ON heartbeat_log (time);

CREATE TABLE ai_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    time         INTEGER NOT NULL, -- unix ms
    harness      TEXT    NOT NULL,
    kind         TEXT    NOT NULL,
    session_id   TEXT    NOT NULL DEFAULT '',
    project      TEXT    NOT NULL DEFAULT '',
    cwd          TEXT    NOT NULL DEFAULT '',
    model        TEXT    NOT NULL DEFAULT '',
    tool         TEXT    NOT NULL DEFAULT '',
    prompt_chars INTEGER NOT NULL DEFAULT 0,
    decision     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX ai_events_time ON ai_events (time);

CREATE TABLE activity_samples (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    time         INTEGER NOT NULL, -- unix ms
    idle_seconds REAL    NOT NULL,
    focus_app    TEXT    NOT NULL DEFAULT '',
    focus_kind   TEXT    NOT NULL
);
CREATE INDEX activity_samples_time ON activity_samples (time);

CREATE TABLE git_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    time          INTEGER NOT NULL, -- unix ms
    project       TEXT    NOT NULL,
    kind          TEXT    NOT NULL,
    commit_hash   TEXT    NOT NULL DEFAULT '',
    files_changed INTEGER NOT NULL,
    insertions    INTEGER NOT NULL,
    deletions     INTEGER NOT NULL
);
CREATE INDEX git_events_time ON git_events (time);
-- A commit is recorded once, however often the git sensor sees it.
CREATE UNIQUE INDEX git_events_commit ON git_events (project, commit_hash) WHERE kind = 'commit';

CREATE TABLE kv (
    key        TEXT PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL -- unix ms
);
