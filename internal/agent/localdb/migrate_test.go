package localdb

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// openAt builds a database at migration version, runs stmts on it, and then
// opens it with Open, which applies the rest.
func openAt(t *testing.T, version int, stmts ...string) *DB {
	t.Helper()
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
	all := []string{qCreateMigrations}
	for _, m := range ms {
		if m.version <= version {
			all = append(all, m.sql, "INSERT INTO schema_migrations VALUES ("+strconv.Itoa(m.version)+", 0)")
		}
	}
	for _, s := range append(all, stmts...) {
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
	return d
}

// Migration 0007 numbers existing rows in arrival order (arrived_at, then
// table, then id), keeps answered credited minutes as final, acknowledged
// and unkeyed, drops unanswered ones, carries the old final watermark over
// as a final span, and marks sealed days legacy.
func TestMigration0007(t *testing.T) {
	ctx := context.Background()
	ms := func(m int) string { return strconv.FormatInt(minute(m).UnixMilli(), 10) }
	sec := func(m int) string { return strconv.FormatInt(minute(m).Unix(), 10) }
	d := openAt(t, 6,
		// Arrivals: activity -10 (id 1), heartbeat -10, ai -20, git -5,
		// activity -30 (id 2), active time -10, repo -15.
		"INSERT INTO activity_samples (time, idle_seconds, focus_kind, arrived_at) VALUES ("+ms(-11)+", 1, 'editor', "+ms(-10)+")",
		"INSERT INTO activity_samples (time, idle_seconds, focus_kind, arrived_at) VALUES ("+ms(-31)+", 1, 'editor', "+ms(-30)+")",
		"INSERT INTO heartbeats (time, fields_hash, payload, arrived_at) VALUES ("+ms(-11)+", x'01', '{}', "+ms(-10)+")",
		"INSERT INTO ai_events (time, harness, kind, arrived_at) VALUES ("+ms(-21)+", 'codex', 'prompt_submit', "+ms(-20)+")",
		"INSERT INTO git_events (time, project, kind, files_changed, insertions, deletions, arrived_at) VALUES ("+ms(-6)+", 'app', 'worktree', 1, 1, 1, "+ms(-5)+")",
		"INSERT INTO active_time (from_ms, to_ms, session_id, type, secs, arrived_at) VALUES ("+ms(-12)+", "+ms(-11)+", 's', 'user', 3, "+ms(-10)+")",
		"INSERT INTO repo_map (since, path, project) VALUES ("+ms(-15)+", '/src/app', 'app')",
		// Credited minutes: answered (final), answered (open, accepted),
		// and one the server never answered for.
		"INSERT INTO credited_minutes (minute, project, label, credited, cuts, flags, rules_version, digest, sent_digest, sent_final, cutoff) VALUES ("+
			sec(-50)+", 'a', 'you', 60, '[]', '[]', 'v1', 'd1', 'd0', 1, 0)",
		"INSERT INTO credited_minutes (minute, project, label, credited, cuts, flags, rules_version, digest, sent_digest, sent_final, cutoff) VALUES ("+
			sec(-10)+", 'a', 'you', 30, '[]', '[]', 'v1', 'd2', 'd2', 0, 0)",
		"INSERT INTO credited_minutes (minute, project, label, credited, cuts, flags, rules_version, digest, sent_digest, sent_final, cutoff) VALUES ("+
			sec(-9)+", 'a', 'you', 30, '[]', '[]', 'v1', 'd3', '', 0, 0)",
		"INSERT INTO kv (key, value, updated_at) VALUES ('crediting.final_through', '"+minute(-40).Format(time.RFC3339)+"', 0)",
		"INSERT INTO credit_cutoffs (from_minute, to_minute, cutoff) VALUES ("+sec(-50)+", "+sec(-45)+", 0)",
		"INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version, "+
			"event_counts, hw_heartbeats, hw_ai, hw_activity, hw_git, state, created_at, updated_at) "+
			"VALUES ('dev', '2026-11-19', '', 'c', 'h', x'01', 'v1', '{}', 1, 1, 1, 1, 'accepted', 0, 0)",
	)
	// Arrival order: activity id 2 (-30), ai (-20), repo (-15), then the
	// ties at -10 by table name (active_time, activity_samples id 1,
	// heartbeats), then git (-5).
	wantSeq := map[string][]int64{
		"activity_samples": {5, 1}, // by id
		"ai_events":        {2},
		"repo_map":         {3},
		"active_time":      {4},
		"heartbeats":       {6},
		"git_events":       {7},
	}
	for table, want := range wantSeq {
		var got []int64
		if err := queryRows(ctx, d.sql, "SELECT seq FROM "+table+" ORDER BY id", nil, func(r *sql.Rows) error { //nolint:gosec // fixed table names
			var s int64
			if err := r.Scan(&s); err != nil {
				return err
			}
			got = append(got, s)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s seq = %v, want %v", table, got, want)
		}
	}
	if h := high(t, d); h != 7 {
		t.Fatalf("log.seq = %d, want 7", h)
	}
	if _, err := d.sql.ExecContext(ctx, "SELECT arrived_at FROM heartbeats"); err == nil {
		t.Fatal("arrived_at still exists")
	}
	rows, err := d.StoredMinutes(ctx, minute(-60), minute(0))
	if err != nil || len(rows) != 2 {
		t.Fatalf("credited minutes = %+v, %v; want the two answered ones", rows, err)
	}
	for _, m := range rows {
		if !m.Final || !m.Acknowledged || !m.Key.IsZero() {
			t.Fatalf("legacy row = %+v, want final, acknowledged and unkeyed", m)
		}
	}
	for _, c := range []struct {
		m    int
		want bool
	}{{-60, true}, {-40, true}, {-10, true}, {-9, false}} {
		if ok, _ := d.IsFinal(ctx, minute(c.m)); ok != c.want {
			t.Errorf("minute %d final = %v, want %v", c.m, ok, c.want)
		}
	}
	if v, _ := d.Get(ctx, "crediting.final_through"); v != "" {
		t.Fatalf("old watermark kept: %q", v)
	}
	e, ok, err := d.LastEvidenceDay(ctx, "dev")
	if err != nil || !ok || !e.Legacy || e.SeqTo != 0 {
		t.Fatalf("legacy day = %+v, %v, %v", e, ok, err)
	}
	if n, _ := d.Counter(ctx, KeySealed); n != 0 {
		t.Fatalf("legacy days sealed rows: %d", n)
	}
	// New rows continue the log.
	if err := d.RecordActivity(ctx, *activity(minute(-1)).Activity); err != nil {
		t.Fatal(err)
	}
	if h := high(t, d); h != 8 {
		t.Fatalf("next row took %d, want 8", h)
	}
}
