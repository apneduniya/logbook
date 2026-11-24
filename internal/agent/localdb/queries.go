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
	qLogTrim   = `DELETE FROM heartbeat_log WHERE id <= (SELECT MAX(id) FROM heartbeat_log) - ?`
	// qRecentDirs lists distinct directories seen in heartbeats and AI events
	// since a time, newest first, with the project name they were seen with.
	qRecentDirs = `SELECT dir, project, MAX(t) AS last FROM (
			SELECT dir, project, time AS t FROM heartbeat_log WHERE time >= ? AND dir <> ''
			UNION ALL
			SELECT cwd AS dir, project, time AS t FROM ai_events WHERE time >= ? AND cwd <> ''
		) GROUP BY dir ORDER BY last DESC LIMIT ?`
)

// Logged inserts (migration 0007): the first parameter is always the row's
// sequence number.
const (
	qHeartbeatInsert = `INSERT OR IGNORE INTO heartbeats (seq, time, fields_hash, payload) VALUES (?, ?, ?, ?)`
	qAIInsert        = `INSERT INTO ai_events (seq, time, harness, kind, session_id, project, cwd, model, tool,
		prompt_chars, decision, prompt_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	qActivityInsert = `INSERT INTO activity_samples (seq, time, idle_seconds, focus_app, focus_kind)
		VALUES (?, ?, ?, ?, ?)`
	qGitInsert = `INSERT OR IGNORE INTO git_events (seq, time, project, kind, commit_hash, files_changed,
		insertions, deletions) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	qActiveInsert = `INSERT OR IGNORE INTO active_time (seq, from_ms, to_ms, session_id, type, secs, repository,
		series_start, cumulative) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	qRepoInsert = `INSERT INTO repo_map (seq, since, path, project) VALUES (?, ?, ?, ?)`
)

const (
	qAICount       = `SELECT COUNT(*) FROM ai_events WHERE time >= ?`
	qActivityCount = `SELECT COUNT(*) FROM activity_samples WHERE time >= ?`
	qGitCount      = `SELECT COUNT(*) FROM git_events WHERE time >= ?`
)

// Retention (ADR 0017): only rows sealed by the device's evidence chain
// (seq at or below the second parameter) are ever deleted.
const (
	qHeartbeatPrune = `DELETE FROM heartbeats WHERE time < ? AND seq <= ?`
	qAIPrune        = `DELETE FROM ai_events WHERE time < ? AND seq <= ?`
	qActivityPrune  = `DELETE FROM activity_samples WHERE time < ? AND seq <= ?`
	qGitPrune       = `DELETE FROM git_events WHERE time < ? AND seq <= ?`
	qActivePrune    = `DELETE FROM active_time WHERE to_ms < ? AND seq <= ?`
	// qRepoPrune drops sealed rows replaced by a newer row for the same
	// path before a time: they apply only to computations older than that.
	qRepoPrune = `DELETE FROM repo_map WHERE seq <= ? AND EXISTS (SELECT 1 FROM repo_map n
		WHERE n.path = repo_map.path AND n.seq > repo_map.seq AND n.since < ?)`

	qHeartbeatTrim = `DELETE FROM heartbeats WHERE id <= (SELECT MAX(id) FROM heartbeats) - ? AND seq <= ? AND time < ?`
	qAITrim        = `DELETE FROM ai_events WHERE id <= (SELECT MAX(id) FROM ai_events) - ? AND seq <= ? AND time < ?`
	qActivityTrim  = `DELETE FROM activity_samples WHERE id <= (SELECT MAX(id) FROM activity_samples) - ? AND seq <= ? AND time < ?`
	qGitTrim       = `DELETE FROM git_events WHERE id <= (SELECT MAX(id) FROM git_events) - ? AND seq <= ? AND time < ?`
	qActiveTrim    = `DELETE FROM active_time WHERE id <= (SELECT MAX(id) FROM active_time) - ? AND seq <= ? AND to_ms < ?`
	// qActivitySpan bounds the activity table's row count from above: ids
	// grow and rows leave from the oldest end.
	qActivitySpan = `SELECT COALESCE(MAX(id) - MIN(id) + 1, 0) FROM activity_samples`
)

// Claude Code active time (migration 0003).
const (
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
	// qSessionSpan is the first and last Claude Code hook event of a session
	// in [from, to) (NULLs when there is none).
	qSessionSpan = `SELECT MIN(time), MAX(time) FROM ai_events
		WHERE session_id = ? AND harness = 'claude-code' AND time >= ? AND time < ?`
	// qUptimeMark records that the receiver run started at from_ms was up
	// at to_ms.
	qUptimeMark = `INSERT INTO otlp_uptime (from_ms, to_ms) VALUES (?, ?)
		ON CONFLICT (from_ms) DO UPDATE SET to_ms = MAX(otlp_uptime.to_ms, excluded.to_ms)`
	// qUptimeRange lists receiver runs overlapping [from, to].
	qUptimeRange = `SELECT from_ms, to_ms FROM otlp_uptime WHERE to_ms >= ? AND from_ms <= ? ORDER BY from_ms, to_ms`
	qUptimePrune = `DELETE FROM otlp_uptime WHERE to_ms < ?`
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

// Log reads for a time range and a range of sequence numbers (after, upto].
// The "ByTime" forms walk the time index (a whole window); the "BySeq"
// forms put "+" before time so SQLite walks the short sequence range of
// rows logged since a previous read instead.
const (
	qHeartbeatsByTime = `SELECT seq, payload FROM heartbeats
		WHERE time >= ? AND time < ? AND seq > ? AND seq <= ? ORDER BY time, seq`
	qAIByTime = `SELECT seq, time, harness, kind, session_id, project, cwd, model, tool, prompt_chars, decision, prompt_hash
		FROM ai_events WHERE time >= ? AND time < ? AND seq > ? AND seq <= ? ORDER BY time, seq`
	qActivityByTime = `SELECT seq, time, idle_seconds, focus_app, focus_kind
		FROM activity_samples WHERE time >= ? AND time < ? AND seq > ? AND seq <= ? ORDER BY time, seq`
	qGitByTime = `SELECT seq, time, project, kind, commit_hash, files_changed, insertions, deletions
		FROM git_events WHERE time >= ? AND time < ? AND seq > ? AND seq <= ? ORDER BY time, seq`

	qHeartbeatsBySeq = `SELECT seq, payload FROM heartbeats
		WHERE +time >= ? AND +time < ? AND seq > ? AND seq <= ? ORDER BY time, seq`
	qAIBySeq = `SELECT seq, time, harness, kind, session_id, project, cwd, model, tool, prompt_chars, decision, prompt_hash
		FROM ai_events WHERE +time >= ? AND +time < ? AND seq > ? AND seq <= ? ORDER BY time, seq`
	qActivityBySeq = `SELECT seq, time, idle_seconds, focus_app, focus_kind
		FROM activity_samples WHERE +time >= ? AND +time < ? AND seq > ? AND seq <= ? ORDER BY time, seq`
	qGitBySeq = `SELECT seq, time, project, kind, commit_hash, files_changed, insertions, deletions
		FROM git_events WHERE +time >= ? AND +time < ? AND seq > ? AND seq <= ? ORDER BY time, seq`

	// qReposAt is each path's latest mapping row at or below a sequence
	// number.
	qReposAt = `SELECT path, project FROM repo_map r WHERE seq = (SELECT MAX(seq) FROM repo_map n
		WHERE n.path = r.path AND n.seq <= ?) ORDER BY path`
	qRepoLatest = `SELECT project FROM repo_map WHERE path = ? ORDER BY seq DESC LIMIT 1`
)

// Whole log rows by sequence range (after, upto], for evidence blocks.
const (
	qHeartbeatRows = `SELECT seq, payload FROM heartbeats WHERE seq > ? AND seq <= ?`
	qAIRows        = `SELECT seq, time, harness, kind, session_id, project, cwd, model, tool, prompt_chars, decision, prompt_hash
		FROM ai_events WHERE seq > ? AND seq <= ?`
	qActivityRows = `SELECT seq, time, idle_seconds, focus_app, focus_kind FROM activity_samples WHERE seq > ? AND seq <= ?`
	qGitRows      = `SELECT seq, time, project, kind, commit_hash, files_changed, insertions, deletions
		FROM git_events WHERE seq > ? AND seq <= ?`
	qActiveRows = `SELECT seq, from_ms, to_ms, session_id, type, secs FROM active_time WHERE seq > ? AND seq <= ?`
	qRepoRows   = `SELECT seq, since, path, project FROM repo_map WHERE seq > ? AND seq <= ?`
	// The same rows by sequence range (?1, ?2] and a time in [?3, ?4): a
	// range of minute buckets (ADR 0019). Active time is timed by its end.
	// "+" before seq makes SQLite walk the (time, seq) index over the short
	// time range, checking seq in the index: a block's sequence range can
	// hold months of rows, and sealing reads it an hour at a time.
	qHeartbeatRowsIn = `SELECT seq, payload FROM heartbeats WHERE +seq > ?1 AND +seq <= ?2 AND time >= ?3 AND time < ?4`
	qAIRowsIn        = `SELECT seq, time, harness, kind, session_id, project, cwd, model, tool, prompt_chars, decision, prompt_hash
		FROM ai_events WHERE +seq > ?1 AND +seq <= ?2 AND time >= ?3 AND time < ?4`
	qActivityRowsIn = `SELECT seq, time, idle_seconds, focus_app, focus_kind FROM activity_samples
		WHERE +seq > ?1 AND +seq <= ?2 AND time >= ?3 AND time < ?4`
	qGitRowsIn = `SELECT seq, time, project, kind, commit_hash, files_changed, insertions, deletions
		FROM git_events WHERE +seq > ?1 AND +seq <= ?2 AND time >= ?3 AND time < ?4`
	qActiveRowsIn = `SELECT seq, from_ms, to_ms, session_id, type, secs FROM active_time
		WHERE +seq > ?1 AND +seq <= ?2 AND to_ms >= ?3 AND to_ms < ?4`
	// qNextRowTime is the earliest time at or after ?3 of a row other than
	// a repo row in the sequence range (?1, ?2]. Each MIN walks a (time,
	// seq) index from ?3 to the first entry in the range, never a row.
	qNextRowTime = `SELECT MIN(t) FROM (
		SELECT MIN(time) AS t FROM heartbeats WHERE time >= ?3 AND +seq > ?1 AND +seq <= ?2 UNION ALL
		SELECT MIN(time) FROM ai_events WHERE time >= ?3 AND +seq > ?1 AND +seq <= ?2 UNION ALL
		SELECT MIN(time) FROM activity_samples WHERE time >= ?3 AND +seq > ?1 AND +seq <= ?2 UNION ALL
		SELECT MIN(time) FROM git_events WHERE time >= ?3 AND +seq > ?1 AND +seq <= ?2 UNION ALL
		SELECT MIN(to_ms) FROM active_time WHERE to_ms >= ?3 AND +seq > ?1 AND +seq <= ?2)`
	// qHasEventRows is 1 when a row other than a repo row has a sequence
	// number in (?1, ?2].
	qHasEventRows = `SELECT EXISTS (SELECT 1 FROM heartbeats WHERE seq > ?1 AND seq <= ?2)
		OR EXISTS (SELECT 1 FROM ai_events WHERE seq > ?1 AND seq <= ?2)
		OR EXISTS (SELECT 1 FROM activity_samples WHERE seq > ?1 AND seq <= ?2)
		OR EXISTS (SELECT 1 FROM git_events WHERE seq > ?1 AND seq <= ?2)
		OR EXISTS (SELECT 1 FROM active_time WHERE seq > ?1 AND seq <= ?2)`
	// qOldestSeq is the smallest sequence number still stored above a mark.
	qOldestSeq = `SELECT MIN(s) FROM (
		SELECT MIN(seq) AS s FROM heartbeats WHERE seq > ?1 UNION ALL
		SELECT MIN(seq) FROM ai_events WHERE seq > ?1 UNION ALL
		SELECT MIN(seq) FROM activity_samples WHERE seq > ?1 UNION ALL
		SELECT MIN(seq) FROM git_events WHERE seq > ?1 UNION ALL
		SELECT MIN(seq) FROM active_time WHERE seq > ?1 UNION ALL
		SELECT MIN(seq) FROM repo_map WHERE seq > ?1)`
)

// Late rows (ADR 0017): rows logged in (after, upto] whose event minute is
// final. One query per raw event table; the table names are constants.
const (
	finalAt = `EXISTS (SELECT 1 FROM final_spans f
		WHERE f.from_minute <= (t.time / 60000) * 60 AND f.to_minute > (t.time / 60000) * 60)`
	qLateHeartbeats = `SELECT COUNT(*), COALESCE(MAX(time), 0) FROM heartbeats t WHERE seq > ? AND seq <= ? AND ` + finalAt
	qLateAI         = `SELECT COUNT(*), COALESCE(MAX(time), 0) FROM ai_events t WHERE seq > ? AND seq <= ? AND ` + finalAt
	qLateActivity   = `SELECT COUNT(*), COALESCE(MAX(time), 0) FROM activity_samples t WHERE seq > ? AND seq <= ? AND ` + finalAt
	qLateGit        = `SELECT COUNT(*), COALESCE(MAX(time), 0) FROM git_events t WHERE seq > ? AND seq <= ? AND ` + finalAt
)

// Credited minutes (migrations 0002 and 0007).
const (
	minuteColumns   = `minute, project, label, credited, cuts, flags, rules_version, digest, sent_digest, computed_at, snapshot, state, acknowledged`
	qMinutesInRange = `SELECT ` + minuteColumns + ` FROM credited_minutes WHERE minute >= ? AND minute < ? ORDER BY minute, project`
	qMinuteInsert   = `INSERT INTO credited_minutes (` + minuteColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	qMinuteClear    = `DELETE FROM credited_minutes WHERE minute = ?`
	// qMinutesOpenBefore lists the minutes with open rows before a minute.
	qMinutesOpenBefore = `SELECT DISTINCT minute FROM credited_minutes WHERE state = 'open' AND minute < ? ORDER BY minute`
	// heldAs is what sent_digest holds once the server has a row exactly as
	// stored: its content and its key.
	heldAs = `digest || '@' || IFNULL(computed_at, '') || '/' || IFNULL(snapshot, '')`
	// qMinutesFinalize makes open rows final. A row the server refused
	// while open is sent again now: only a row the server holds as is
	// stays acknowledged.
	qMinutesFinalize = `UPDATE credited_minutes SET state = 'final', acknowledged = (sent_digest = ` + heldAs + `)
		WHERE state = 'open' AND minute < ?`
	qMinuteHeld = `UPDATE credited_minutes SET sent_digest = ` + heldAs + ` WHERE minute = ?`
	// qMinutesUnsent returns unacknowledged rows of minutes after ?1, up to
	// and including the minute of the (?3+1)-th one, so that minute comes
	// whole. Open minutes before ?2 (held while settling) are left out.
	unsentRows     = `acknowledged = 0 AND minute > ?1 AND NOT (state = 'open' AND minute < ?2)`
	qMinutesUnsent = `SELECT ` + minuteColumns + ` FROM credited_minutes WHERE ` + unsentRows + `
		AND minute <= COALESCE((SELECT minute FROM credited_minutes WHERE ` + unsentRows + `
			ORDER BY minute, credited, project LIMIT 1 OFFSET ?3), 9223372036854775807)
		ORDER BY minute, credited, project`
	// rowIs matches a row exactly as it was read: content and key.
	rowIs = ` WHERE minute = ? AND project = ? AND digest = ? AND computed_at IS ? AND snapshot IS ?`
	// qMinuteAccepted records that the server holds the row; qMinuteRemoved
	// then deletes it if it was a removal (idle, no credit, cuts or flags:
	// the server deleted its row, so there is nothing left to hold).
	qMinuteAccepted = `UPDATE credited_minutes SET acknowledged = 1, sent_digest = ` + heldAs + rowIs
	qMinuteRemoved  = `DELETE FROM credited_minutes` + rowIs +
		` AND acknowledged = 1 AND label = 'idle' AND credited = 0 AND cuts = '[]' AND flags = '[]'`
	qMinuteAnswered = `UPDATE credited_minutes SET acknowledged = 1` + rowIs
	// qMinuteDropped deletes a row the server refused for good.
	qMinuteDropped = `DELETE FROM credited_minutes` + rowIs
	// qMinuteTried notes that a row was sent and the server may hold it,
	// before the answer is known ('?' never equals heldAs).
	qMinuteTried = `UPDATE credited_minutes SET sent_digest = '?' WHERE minute = ? AND project = ? AND sent_digest = ''`
	// qMinutesRetireUnheld deletes rows before a minute that the server
	// doesn't hold exactly as stored, except rows from before keys
	// (migration 0007), which it holds as far as the agent can know;
	// qMinutesRetire then makes the rest final and acknowledged
	// (RetireMinutesBefore).
	qMinutesRetireUnheld = `DELETE FROM credited_minutes WHERE minute < ? AND sent_digest <> ` + heldAs + `
		AND NOT (state = 'final' AND acknowledged = 1 AND computed_at IS NULL)`
	qMinutesRetire     = `UPDATE credited_minutes SET state = 'final', acknowledged = 1 WHERE minute < ?`
	qMinutesLastBefore = `SELECT MAX(minute) FROM credited_minutes WHERE minute < ?`
	qMinutesUnacked    = `SELECT COUNT(*) FROM credited_minutes WHERE acknowledged = 0`
	// qMinutesPendingBefore counts rows before a minute that are open or
	// the server hasn't answered for yet.
	qMinutesPendingBefore = `SELECT COUNT(*) FROM credited_minutes
		WHERE minute < ? AND (acknowledged = 0 OR state = 'open')`
	qMinutesPrune = `DELETE FROM credited_minutes WHERE minute < ?`
)

// Final spans (migration 0007).
const (
	// qSpansTouching lists spans overlapping or adjacent to [?, ?].
	qSpansTouching = `SELECT from_minute, to_minute FROM final_spans WHERE to_minute >= ? AND from_minute <= ?`
	qSpansOverlap  = `SELECT from_minute, to_minute FROM final_spans WHERE to_minute > ? AND from_minute < ? ORDER BY from_minute`
	qSpanDelete    = `DELETE FROM final_spans WHERE from_minute = ?`
	qSpanInsert    = `INSERT INTO final_spans (from_minute, to_minute) VALUES (?, ?)`
	qSpansPrune    = `DELETE FROM final_spans WHERE to_minute <= ?`
)

// Evidence days and blocks (migrations 0002, 0007 and 0008).
const (
	qEvidenceInsert = `INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash,
		signature, rules_version, event_counts, seq_from, seq_to, buckets, time_from, time_to, legacy, state, error,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`
	qEvidenceColumns = `SELECT device_id, day, prev_chain_hash, content_hash, chain_hash, signature,
		rules_version, event_counts, seq_from, seq_to, buckets, time_from, time_to, legacy, state, error,
		created_at, updated_at FROM evidence_days`
	qEvidenceLast     = qEvidenceColumns + ` WHERE device_id = ? ORDER BY day DESC LIMIT 1`
	qEvidenceDay      = qEvidenceColumns + ` WHERE device_id = ? AND day = ?`
	qEvidenceAll      = qEvidenceColumns + ` WHERE device_id = ? ORDER BY day`
	qEvidenceSetState = `UPDATE evidence_days SET state = ?, error = ?, updated_at = ? WHERE device_id = ? AND day = ?`

	qBucketInsert = `INSERT INTO evidence_buckets (device_id, day, i, key, salt, bucket_hash) VALUES (?, ?, ?, ?, ?, ?)`
	qBucketList   = `SELECT i, key, salt, bucket_hash FROM evidence_buckets WHERE device_id = ? AND day = ? ORDER BY i`
	qBucketsDrop  = `DELETE FROM evidence_buckets WHERE device_id = ? AND day = ?`

	qSealingInsert = `INSERT INTO evidence_sealing (id, device_id, day, prev_chain_hash, seq_from, seq_to, reserved_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)`
	qSealingGet = `SELECT device_id, day, prev_chain_hash, seq_from, seq_to, cursor, hasher, buckets, reserved_at
		FROM evidence_sealing WHERE id = 1`
	qSealingStep   = `UPDATE evidence_sealing SET cursor = ?, hasher = ?, buckets = ? WHERE id = 1`
	qSealingDelete = `DELETE FROM evidence_sealing WHERE id = 1`

	qBaselineSet = `INSERT INTO evidence_baselines (device_id, seq_from) VALUES (?, ?)
		ON CONFLICT (device_id) DO UPDATE SET seq_from = excluded.seq_from`
	qBaselineGet = `SELECT seq_from FROM evidence_baselines WHERE device_id = ?`

	// qRepoSnapshot logs each path's latest mapping at ?1 again, in path
	// order, as new rows dated ?2 (ADR 0019 point 4); the caller numbers
	// them.
	qRepoSnapshotRows = `SELECT path, project FROM repo_map r WHERE seq = (SELECT MAX(seq) FROM repo_map n
		WHERE n.path = r.path AND n.seq <= ?) ORDER BY path`
)
