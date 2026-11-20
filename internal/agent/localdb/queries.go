package localdb

// SQL lives here, in one place, as constants. The agent's schema is small
// and SQLite-specific, so it does not use the server's sqlc setup.

const (
	qCreateMigrations = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`
	qCurrentVersion  = `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`
	qInsertMigration = `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`
)

// Upload queues. The table name is a constant per queue, never user input.
const (
	qHBInsert  = `INSERT INTO heartbeat_queue (enqueued_at, payload) VALUES (?, ?)`
	qHBPeek    = `SELECT id, payload FROM heartbeat_queue ORDER BY id LIMIT ?`
	qHBDelete  = `DELETE FROM heartbeat_queue WHERE id = ?`
	qHBCount   = `SELECT COUNT(*) FROM heartbeat_queue`
	qHBTrim    = `DELETE FROM heartbeat_queue WHERE id <= (SELECT MAX(id) FROM heartbeat_queue) - ?`
	qHBOldest  = `SELECT MIN(enqueued_at) FROM heartbeat_queue`
	qMirInsert = `INSERT INTO mirror_queue (enqueued_at, payload) VALUES (?, ?)`
	qMirPeek   = `SELECT id, payload FROM mirror_queue ORDER BY id LIMIT ?`
	qMirDelete = `DELETE FROM mirror_queue WHERE id = ?`
	qMirCount  = `SELECT COUNT(*) FROM mirror_queue`
	qMirTrim   = `DELETE FROM mirror_queue WHERE id <= (SELECT MAX(id) FROM mirror_queue) - ?`
	qMirOldest = `SELECT MIN(enqueued_at) FROM mirror_queue`
)

const (
	qLogInsert = `INSERT INTO heartbeat_log (time, project, dir) VALUES (?, ?, ?)`
	qLogTimes  = `SELECT time FROM heartbeat_log WHERE time >= ? ORDER BY time`
	qLogPrune  = `DELETE FROM heartbeat_log WHERE time < ?`
	qLogCount  = `SELECT COUNT(*) FROM heartbeat_log WHERE time >= ?`
	// qRecentDirs lists distinct directories seen in heartbeats and AI events
	// since a time, newest first, with the project name they were seen with.
	qRecentDirs = `SELECT dir, project, MAX(t) AS last FROM (
			SELECT dir, project, time AS t FROM heartbeat_log WHERE time >= ? AND dir <> ''
			UNION ALL
			SELECT cwd AS dir, project, time AS t FROM ai_events WHERE time >= ? AND cwd <> ''
		) GROUP BY dir ORDER BY last DESC LIMIT ?`
)

const (
	qAIInsert = `INSERT INTO ai_events (time, harness, kind, session_id, project, cwd, model, tool,
		prompt_chars, decision, prompt_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	qActivityInsert = `INSERT INTO activity_samples (time, idle_seconds, focus_app, focus_kind)
		VALUES (?, ?, ?, ?)`
	qGitInsert = `INSERT OR IGNORE INTO git_events (time, project, kind, commit_hash, files_changed,
		insertions, deletions) VALUES (?, ?, ?, ?, ?, ?, ?)`

	qAICount       = `SELECT COUNT(*) FROM ai_events WHERE time >= ?`
	qActivityCount = `SELECT COUNT(*) FROM activity_samples WHERE time >= ?`
	qGitCount      = `SELECT COUNT(*) FROM git_events WHERE time >= ?`

	qAIPrune       = `DELETE FROM ai_events WHERE time < ?`
	qActivityPrune = `DELETE FROM activity_samples WHERE time < ?`
	qGitPrune      = `DELETE FROM git_events WHERE time < ?`

	qAITrim       = `DELETE FROM ai_events WHERE id <= (SELECT MAX(id) FROM ai_events) - ?`
	qActivityTrim = `DELETE FROM activity_samples WHERE id <= (SELECT MAX(id) FROM activity_samples) - ?`
	qGitTrim      = `DELETE FROM git_events WHERE id <= (SELECT MAX(id) FROM git_events) - ?`
	qLogTrim      = `DELETE FROM heartbeat_log WHERE id <= (SELECT MAX(id) FROM heartbeat_log) - ?`
)

// Claude Code active time (migration 0003).
const (
	qActiveInsert = `INSERT OR IGNORE INTO active_time (from_ms, to_ms, session_id, type, secs, repository,
		series_start, cumulative) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	// qActiveLastCumulative is a cumulative series' latest stored point.
	qActiveLastCumulative = `SELECT to_ms, cumulative FROM active_time
		WHERE session_id = ? AND type = ? AND series_start = ? AND cumulative IS NOT NULL
		ORDER BY to_ms DESC LIMIT 1`
	// qActiveRange lists points overlapping [from, to); a zero-length point
	// counts when it lies inside. Zero-second points (kept only to track
	// cumulative series) are left out. Parameters: from, to, from.
	qActiveRange = `SELECT from_ms, to_ms, session_id, type, secs FROM active_time
		WHERE to_ms >= ? AND from_ms < ? AND (to_ms > ? OR to_ms = from_ms) AND secs > 0
		ORDER BY from_ms, to_ms, session_id, type`
	// qSessionCwd is the latest working directory Claude Code's hooks
	// reported for a session before a time.
	qSessionCwd = `SELECT cwd FROM ai_events
		WHERE session_id = ? AND harness = 'claude-code' AND cwd <> '' AND time < ?
		ORDER BY time DESC LIMIT 1`
	qActivePrune = `DELETE FROM active_time WHERE to_ms < ?`
	qActiveTrim  = `DELETE FROM active_time WHERE id <= (SELECT MAX(id) FROM active_time) - ?`
)

const (
	qKVGet = `SELECT value FROM kv WHERE key = ?`
	qKVSet = `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
	qKVAdd = `INSERT INTO kv (key, value, updated_at) VALUES (?, CAST(? AS TEXT), ?)
		ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(kv.value AS INTEGER) + CAST(excluded.value AS INTEGER) AS TEXT),
		updated_at = excluded.updated_at`
	qKVPrefix = `SELECT key, value FROM kv WHERE key >= ? AND key < ? ORDER BY key`
)
