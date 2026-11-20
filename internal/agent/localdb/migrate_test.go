package localdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Migration 0006 gives rows stored before it, whose arrival is unknown,
// their own event time as the arrival, and credited minutes the cutoff the
// old pipeline last rewrote them at (minute + 29 minutes).
func TestMigration0006Backfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "logbook.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{qCreateMigrations}
	for _, m := range ms {
		if m.version < 6 {
			stmts = append(stmts, m.sql, "INSERT INTO schema_migrations VALUES ("+strconv.Itoa(m.version)+", 0)")
		}
	}
	at := testNow.Add(-time.Hour).UnixMilli()
	minute := testNow.Add(-time.Hour).Unix()
	stmts = append(stmts,
		"INSERT INTO activity_samples (time, idle_seconds, focus_kind) VALUES ("+strconv.FormatInt(at, 10)+", 1, 'editor')",
		"INSERT INTO heartbeats (time, fields_hash, payload) VALUES ("+strconv.FormatInt(at, 10)+", x'01', '{}')",
		"INSERT INTO ai_events (time, harness, kind) VALUES ("+strconv.FormatInt(at, 10)+", 'codex', 'prompt_submit')",
		"INSERT INTO credited_minutes (minute, project, label, credited, cuts, flags, rules_version, digest) VALUES ("+
			strconv.FormatInt(minute, 10)+", 'app', 'you', 60, '[]', '[]', 'v1', 'd')",
	)
	for _, s := range stmts {
		if _, err := raw.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	_ = raw.Close()

	d, err := Open(ctx, path, WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, table := range []string{"activity_samples", "heartbeats", "ai_events"} {
		var arrived int64
		if err := d.sql.QueryRowContext(ctx, "SELECT arrived_at FROM "+table).Scan(&arrived); err != nil || arrived != at {
			t.Fatalf("%s arrived_at = %d, %v; want the event time %d", table, arrived, err, at)
		}
	}
	rows, err := d.StoredMinutes(ctx, time.Unix(minute, 0), time.Unix(minute+60, 0))
	if err != nil || len(rows) != 1 || !rows[0].Cutoff.Equal(time.Unix(minute, 0).Add(29*time.Minute)) {
		t.Fatalf("credited minute after migration = %+v, %v", rows, err)
	}
}
