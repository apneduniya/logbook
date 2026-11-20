-- Keyed prompt hash of prompt_submit events (wire.AIEvent.PromptHash), so the
-- struggle engine can spot repeated prompts. Never the prompt itself.
ALTER TABLE ai_events ADD COLUMN prompt_hash TEXT NOT NULL DEFAULT '';

-- Finds a Claude Code session's working directory from its hook events, to
-- give OpenTelemetry active time (which knows only the session id) a project.
CREATE INDEX ai_events_session ON ai_events (session_id, time);

-- Claude Code's own active-time measure (claude_code.active_time.total),
-- received over local OTLP/HTTP and stored as deltas: secs seconds of type
-- 'user' or 'cli' during [from_ms, to_ms). Only these fields are kept; every
-- identity attribute (user.email, account and organization ids, user.id,
-- terminal and host details) is dropped before storage.
--
-- For a cumulative series, series_start is the series' start time and
-- cumulative its running total at to_ms, so the next point (even after an
-- agent restart) can be turned into a delta; both are 0 and NULL for points
-- that arrived as deltas.
CREATE TABLE active_time (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    from_ms      INTEGER NOT NULL, -- unix ms
    to_ms        INTEGER NOT NULL, -- unix ms
    session_id   TEXT    NOT NULL,
    type         TEXT    NOT NULL CHECK (type IN ('user', 'cli')),
    secs         REAL    NOT NULL CHECK (secs >= 0),
    repository   TEXT    NOT NULL DEFAULT '', -- vcs.repository.name, when Claude Code sends it
    series_start INTEGER NOT NULL DEFAULT 0,  -- unix ms; cumulative series only
    cumulative   REAL                         -- running total at to_ms; cumulative series only
);
CREATE INDEX active_time_to ON active_time (to_ms);
-- A retried export carries the same intervals; it is stored once.
CREATE UNIQUE INDEX active_time_point ON active_time (session_id, type, from_ms, to_ms);
CREATE INDEX active_time_series ON active_time (session_id, type, series_start, to_ms);
