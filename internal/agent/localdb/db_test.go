package localdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

var testNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func openTest(t *testing.T, opts ...Option) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logbook.db")
	opts = append([]Option{WithClock(func() time.Time { return testNow })}, opts...)
	d, err := Open(t.Context(), path, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, path
}

// srcRoot is an absolute directory on every OS (with a drive on Windows).
func srcRoot() string {
	return filepath.VolumeName(os.TempDir()) + string(filepath.Separator) + "src"
}

func hb(i int) wire.Heartbeat {
	return wire.Heartbeat{
		Entity: filepath.Join(srcRoot(), "app", "main.go"), Type: "file",
		Time: float64(testNow.Unix()) - float64(1000-i), Project: "app",
	}
}

func TestOpenMigratesAndReopens(t *testing.T) {
	d, path := openTest(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("db mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	// Reopening applies nothing new and keeps working.
	d2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = d2.Close() }()
	if _, err := d2.sql.ExecContext(context.Background(), `INSERT INTO schema_migrations VALUES (999, 0)`); err != nil {
		t.Fatal(err)
	}
	_ = d2.Close()
	if _, err := Open(context.Background(), path); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Open with newer schema err = %v, want ErrNewerSchema", err)
	}
}

func TestQueueRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		mirror     bool
		wantMirror int64
	}{
		{"logbook only", false, 0},
		{"with mirror", true, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			d, _ := openTest(t)
			if err := d.Enqueue(ctx, []wire.Heartbeat{hb(1), hb(2), hb(3)}, tt.mirror); err != nil {
				t.Fatal(err)
			}
			if n, _ := d.Pending(ctx, HeartbeatQueue); n != 3 {
				t.Fatalf("heartbeat queue = %d, want 3", n)
			}
			if n, _ := d.Pending(ctx, MirrorQueue); n != tt.wantMirror {
				t.Fatalf("mirror queue = %d, want %d", n, tt.wantMirror)
			}
			items, err := d.Peek(ctx, HeartbeatQueue, 2)
			if err != nil || len(items) != 2 {
				t.Fatalf("Peek = %v, %v", items, err)
			}
			if items[0].Heartbeat.Time != hb(1).Time || items[1].Heartbeat.Time != hb(2).Time {
				t.Fatal("Peek is not oldest-first")
			}
			if err := d.Delete(ctx, HeartbeatQueue, []int64{items[0].ID, items[1].ID}); err != nil {
				t.Fatal(err)
			}
			st, err := d.Stats(ctx, HeartbeatQueue)
			if err != nil || st.Pending != 1 || st.Oldest.IsZero() {
				t.Fatalf("Stats = %+v, %v", st, err)
			}
		})
	}
}

func TestQueueCapDropsOldest(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t, WithQueueCap(5))
	for i := range 3 {
		batch := []wire.Heartbeat{hb(i*3 + 0), hb(i*3 + 1), hb(i*3 + 2)}
		if err := d.Enqueue(ctx, batch, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []Queue{HeartbeatQueue, MirrorQueue} {
		st, err := d.Stats(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending != 5 || st.Dropped != 4 {
			t.Fatalf("%s stats = %+v, want 5 pending, 4 dropped", q, st)
		}
		items, _ := d.Peek(ctx, q, 10)
		if items[0].Heartbeat.Time != hb(4).Time {
			t.Fatalf("%s kept the wrong rows: first = %v", q, items[0].Heartbeat.Time)
		}
	}
}

func TestPeekDropsCorruptRows(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	if _, err := d.sql.ExecContext(ctx, qHBInsert, 0, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(ctx, []wire.Heartbeat{hb(1)}, false); err != nil {
		t.Fatal(err)
	}
	items, err := d.Peek(ctx, HeartbeatQueue, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("Peek = %v, %v", items, err)
	}
	if n, _ := d.Pending(ctx, HeartbeatQueue); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}
}

func TestEventsCountsAndPrune(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	recent := testNow.Add(-time.Hour)
	old := testNow.Add(-100 * 24 * time.Hour)
	commit := wire.GitEvent{Time: recent, Project: "app", Kind: "commit", CommitHash: "abc", FilesChanged: 1}
	evs := []wire.LocalEvent{
		{Type: "ai", AI: &wire.AIEvent{Time: recent, Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit,
			Cwd: "/src/app", Project: "app", PromptChars: 12}},
		{Type: "ai", AI: &wire.AIEvent{Time: old, Harness: wire.HarnessCodex, Kind: wire.AITurnEnd}},
		{Type: "activity", Activity: &wire.ActivitySample{Time: recent, FocusKind: "editor"}},
		{Type: "git", Git: &commit},
		{Type: "git", Git: &commit}, // duplicate commit is ignored
	}
	if err := d.InsertEvents(ctx, evs, KV{"spool.offset", "42"}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordGit(ctx, commit); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.Get(ctx, "spool.offset"); v != "42" {
		t.Fatalf("mark = %q", v)
	}
	c, err := d.CountSince(ctx, testNow.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c != (Counts{AIEvents: 1, ActivitySamples: 1, GitEvents: 1}) {
		t.Fatalf("counts = %+v", c)
	}
	dirs, err := d.RecentDirs(ctx, testNow.Add(-2*time.Hour), 10)
	if err != nil || len(dirs) != 1 || dirs[0].Dir != "/src/app" || dirs[0].Project != "app" {
		t.Fatalf("RecentDirs = %+v, %v", dirs, err)
	}
	// Old events go once the evidence chain has sealed them.
	seal(t, d, high(t, d))
	r, err := d.Prune(ctx)
	if err != nil || r.AIEvents != 1 {
		t.Fatalf("Prune = %+v, %v", r, err)
	}
	bad := []wire.LocalEvent{{Type: "ai", Activity: &wire.ActivitySample{}}}
	if err := d.InsertEvents(ctx, bad); !errors.Is(err, wire.ErrInvalidEvent) {
		t.Fatalf("invalid event err = %v", err)
	}
}

func TestKV(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	for range 3 {
		if err := d.Add(ctx, "dropped.x", 2); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := d.Counter(ctx, "dropped.x"); err != nil || n != 6 {
		t.Fatalf("Counter = %d, %v", n, err)
	}
	if err := d.SetMany(ctx, KV{"upload.a.x", "1"}, KV{"upload.a.y", "2"}, KV{"upload.b", "3"}); err != nil {
		t.Fatal(err)
	}
	m, err := d.Prefix(ctx, "upload.a.")
	if err != nil || len(m) != 2 || m["upload.a.y"] != "2" {
		t.Fatalf("Prefix = %v, %v", m, err)
	}
	if v, err := d.Get(ctx, "missing"); err != nil || v != "" {
		t.Fatalf("Get missing = %q, %v", v, err)
	}
}

func TestHeartbeatLogEntityDir(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	app := hb(1)
	app.Type = "app"
	rel := hb(2)
	rel.Entity = "relative.go"
	if err := d.Enqueue(ctx, []wire.Heartbeat{hb(0), app, rel}, false); err != nil {
		t.Fatal(err)
	}
	dirs, err := d.RecentDirs(ctx, time.Time{}, 10)
	if err != nil || len(dirs) != 1 || dirs[0].Dir != filepath.Dir(hb(0).Entity) {
		t.Fatalf("RecentDirs = %+v, %v", dirs, err)
	}
	times, err := d.HeartbeatTimes(ctx, time.Time{})
	if err != nil || len(times) != 3 {
		t.Fatalf("HeartbeatTimes = %v, %v", times, err)
	}
}
