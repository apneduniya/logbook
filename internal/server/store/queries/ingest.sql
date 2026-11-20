-- Queries for internal/server/ingest. Every query is scoped to one user_id.

-- name: LockUserIngest :exec
-- Serialises ingest batches for one user until the transaction ends.
-- 1001 namespaces the lock so it can't collide with other advisory locks.
SELECT pg_advisory_xact_lock(1001, hashtext(sqlc.arg(user_id)::uuid::text));

-- name: InsertHeartbeats :many
-- Inserts a batch in one round trip. rows is a JSON array of objects whose
-- keys match the column list below (fields_hash hex-encoded, time RFC 3339).
-- Duplicates are skipped; projects rows are created for new project names.
-- Returns the time of each heartbeat that was actually inserted.
WITH inserted AS (
    INSERT INTO heartbeats (
        user_id, time, fields_hash, source, entity, type, category, project, branch,
        language, editor, plugin, machine, operating_system, user_agent, is_write,
        lines, lineno, cursorpos, line_additions, line_deletions, project_root_count,
        dependencies, ai_model, ai_session, ai_input_tokens, ai_output_tokens,
        ai_prompt_length, ai_line_changes, human_line_changes
    )
    SELECT
        sqlc.arg(user_id)::uuid, b.time, decode(b.fields_hash, 'hex'), sqlc.arg(source)::text,
        b.entity, b.type, b.category, b.project, b.branch, b.language, b.editor, b.plugin,
        b.machine, b.operating_system, b.user_agent, b.is_write, b.lines, b.lineno,
        b.cursorpos, b.line_additions, b.line_deletions, b.project_root_count,
        coalesce(b.dependencies, '{}'), b.ai_model, b.ai_session, b.ai_input_tokens,
        b.ai_output_tokens, b.ai_prompt_length, b.ai_line_changes, b.human_line_changes
    FROM jsonb_to_recordset(sqlc.arg(rows)::jsonb) AS b(
        time timestamptz, fields_hash text, entity text, type text, category text,
        project text, branch text, language text, editor text, plugin text,
        machine text, operating_system text, user_agent text, is_write boolean,
        lines integer, lineno integer, cursorpos integer, line_additions integer,
        line_deletions integer, project_root_count integer, dependencies text[],
        ai_model text, ai_session text, ai_input_tokens bigint, ai_output_tokens bigint,
        ai_prompt_length integer, ai_line_changes integer, human_line_changes integer
    )
    ON CONFLICT (user_id, fields_hash, time) DO NOTHING
    RETURNING heartbeats.time, heartbeats.project
),
new_projects AS (
    INSERT INTO projects (user_id, name)
    SELECT DISTINCT sqlc.arg(user_id)::uuid, i.project
    FROM inserted i
    WHERE i.project <> ''
    ON CONFLICT (user_id, name) DO NOTHING
)
SELECT inserted.time FROM inserted;

-- name: ListRollupHeartbeats :many
-- Reads the heartbeats a rollup recompute needs. The ranges are inclusive and
-- never overlap, so no row is returned twice. min_time/max_time bound the scan
-- so Postgres prunes partitions outside the batch.
SELECT h.time, h.project, h.language, h.editor, h.category
FROM heartbeats h
JOIN (
    SELECT unnest(sqlc.arg(range_starts)::timestamptz[]) AS lo,
           unnest(sqlc.arg(range_ends)::timestamptz[]) AS hi
) r
    ON h.time >= r.lo AND h.time <= r.hi
WHERE h.user_id = sqlc.arg(user_id)
  AND h.time >= sqlc.arg(min_time)
  AND h.time <= sqlc.arg(max_time)
ORDER BY h.time, h.fields_hash;

-- name: ReplaceUserMinutes :exec
-- Makes the 'you' rows inside the given windows equal to the supplied set in
-- one round trip: upserts the fresh rows and deletes every other 'you' row in
-- the windows. Both parts see the same snapshot and touch disjoint rows.
WITH fresh AS (
    -- Several set-returning functions in one SELECT list are zipped row by row.
    SELECT
        unnest(sqlc.arg(minutes)::timestamptz[]) AS minute,
        unnest(sqlc.arg(projects)::text[]) AS project,
        unnest(sqlc.arg(languages)::text[]) AS language,
        unnest(sqlc.arg(editors)::text[]) AS editor,
        unnest(sqlc.arg(categories)::text[]) AS category,
        unnest(sqlc.arg(seconds)::smallint[]) AS seconds
),
upserted AS (
    INSERT INTO user_minutes (user_id, minute, project, language, editor, category, label, seconds)
    SELECT sqlc.arg(user_id)::uuid, f.minute, f.project, f.language, f.editor, f.category, 'you', f.seconds
    FROM fresh f
    ON CONFLICT (user_id, minute, project, language, editor, category, label)
    DO UPDATE SET seconds = EXCLUDED.seconds
)
DELETE FROM user_minutes um
USING (
    SELECT unnest(sqlc.arg(window_starts)::timestamptz[]) AS lo,
           unnest(sqlc.arg(window_ends)::timestamptz[]) AS hi
) w
WHERE um.user_id = sqlc.arg(user_id)::uuid
  AND um.label = 'you'
  AND um.minute >= w.lo
  AND um.minute < w.hi
  AND NOT EXISTS (
      SELECT 1 FROM fresh f
      WHERE f.minute = um.minute AND f.project = um.project AND f.language = um.language
        AND f.editor = um.editor AND f.category = um.category
  );

-- name: SumUserMinutesByGroup :many
-- Totals a user's 'you' seconds in [start_time, end_time) per rollup group.
SELECT project, language, editor, category, sum(seconds)::bigint AS seconds
FROM user_minutes
WHERE user_id = sqlc.arg(user_id)
  AND label = 'you'
  AND minute >= sqlc.arg(start_time)
  AND minute < sqlc.arg(end_time)
GROUP BY project, language, editor, category;
