-- The repo mapping (repository path -> project name) the struggle engine
-- uses, as an append-only journal: a row is added when a path is first seen
-- or its project changes, and never updated. A minute is computed with each
-- path's latest row (by id) whose since is before the minute became final
-- (30 minutes after it started), so its project doesn't depend on when it is
-- recomputed, and a replay can rebuild the mapping it used from these rows.
CREATE TABLE repo_map (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    since   INTEGER NOT NULL, -- unix ms, when the agent first saw this mapping
    path    TEXT    NOT NULL,
    project TEXT    NOT NULL
);
CREATE INDEX repo_map_path ON repo_map (path, id);
