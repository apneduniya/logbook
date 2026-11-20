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
		prompt_chars, decision) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
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

const (
	qKVGet = `SELECT value FROM kv WHERE key = ?`
	qKVSet = `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
	qKVAdd = `INSERT INTO kv (key, value, updated_at) VALUES (?, CAST(? AS TEXT), ?)
		ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(kv.value AS INTEGER) + CAST(excluded.value AS INTEGER) AS TEXT),
		updated_at = excluded.updated_at`
	qKVPrefix = `SELECT key, value FROM kv WHERE key >= ? AND key < ? ORDER BY key`
)

// Full heartbeats (migration 0002).
const (
	qHeartbeatInsert = `INSERT OR IGNORE INTO heartbeats (time, fields_hash, payload) VALUES (?, ?, ?)`
	qHeartbeatTrim   = `DELETE FROM heartbeats WHERE id <= (SELECT MAX(id) FROM heartbeats) - ?`
	qHeartbeatPrune  = `DELETE FROM heartbeats WHERE time < ?`
)

// Event reads for a time range and a range of row ids (migration 0002).
const (
	qHighWater = `SELECT
		(SELECT COALESCE(MAX(id), 0) FROM heartbeats),
		(SELECT COALESCE(MAX(id), 0) FROM ai_events),
		(SELECT COALESCE(MAX(id), 0) FROM activity_samples),
		(SELECT COALESCE(MAX(id), 0) FROM git_events)`
	qHeartbeatsRange = `SELECT payload FROM heartbeats WHERE time >= ? AND time < ? AND id > ? AND id <= ? ORDER BY time, id`
	qAIRange         = `SELECT time, harness, kind, session_id, project, cwd, model, tool, prompt_chars, decision
		FROM ai_events WHERE time >= ? AND time < ? AND id > ? AND id <= ? ORDER BY time, id`
	qActivityRange = `SELECT time, idle_seconds, focus_app, focus_kind
		FROM activity_samples WHERE time >= ? AND time < ? AND id > ? AND id <= ? ORDER BY time, id`
	qGitRange = `SELECT time, project, kind, commit_hash, files_changed, insertions, deletions
		FROM git_events WHERE time >= ? AND time < ? AND id > ? AND id <= ? ORDER BY time, id`
)

// Credited minutes (migration 0002).
const (
	qMinutesInRange = `SELECT minute, project, digest, sent_digest FROM credited_minutes
		WHERE minute >= ? AND minute < ?`
	qMinuteInsert = `INSERT INTO credited_minutes (minute, project, label, credited, cuts, flags,
		rules_version, digest) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	qMinuteUpdate = `UPDATE credited_minutes SET label = ?, credited = ?, cuts = ?, flags = ?,
		rules_version = ?, digest = ? WHERE minute = ? AND project = ?`
	qMinuteDelete = `DELETE FROM credited_minutes WHERE minute = ? AND project = ?`
	// qMinutesSettleFinal marks final minutes whose content the server
	// already has as done, without sending them again.
	qMinutesSettleFinal = `UPDATE credited_minutes SET sent_final = 1
		WHERE sent_final = 0 AND minute < ? AND digest = sent_digest`
	qMinutesUnsent = `SELECT minute, project, label, credited, cuts, flags, rules_version, digest
		FROM credited_minutes WHERE sent_final = 0 AND digest <> sent_digest
		ORDER BY minute, credited, project LIMIT ?`
	qMinuteMarkSent = `UPDATE credited_minutes SET sent_digest = digest, sent_final = ?
		WHERE minute = ? AND project = ? AND digest = ?`
	qMinutesRetire      = `UPDATE credited_minutes SET sent_final = 1 WHERE sent_final = 0 AND minute < ?`
	qMinutesUnsentCount = `SELECT COUNT(*) FROM credited_minutes WHERE sent_final = 0 AND digest <> sent_digest`
	qMinutesPrune       = `DELETE FROM credited_minutes WHERE minute < ?`
)

// Evidence days (migration 0002).
const (
	qEvidenceInsert = `INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash,
		signature, rules_version, event_counts, hw_heartbeats, hw_ai, hw_activity, hw_git, state, error,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	qEvidenceLast = `SELECT device_id, day, prev_chain_hash, content_hash, chain_hash, signature,
		rules_version, event_counts, hw_heartbeats, hw_ai, hw_activity, hw_git, state, error, created_at, updated_at
		FROM evidence_days WHERE device_id = ? ORDER BY day DESC LIMIT 1`
	qEvidenceSetState = `UPDATE evidence_days SET state = ?, error = ?, updated_at = ? WHERE device_id = ? AND day = ?`
)

// The same event reads for new rows only: "+time" stops SQLite from using
// the time index, so it walks the short id range instead of the whole time
// range.
const (
	qHeartbeatsAfter = `SELECT payload FROM heartbeats WHERE +time >= ? AND +time < ? AND id > ? AND id <= ? ORDER BY time, id`
	qAIAfter         = `SELECT time, harness, kind, session_id, project, cwd, model, tool, prompt_chars, decision
		FROM ai_events WHERE +time >= ? AND +time < ? AND id > ? AND id <= ? ORDER BY time, id`
	qActivityAfter = `SELECT time, idle_seconds, focus_app, focus_kind
		FROM activity_samples WHERE +time >= ? AND +time < ? AND id > ? AND id <= ? ORDER BY time, id`
	qGitAfter = `SELECT time, project, kind, commit_hash, files_changed, insertions, deletions
		FROM git_events WHERE +time >= ? AND +time < ? AND id > ? AND id <= ? ORDER BY time, id`
)
