package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/spoolfile"
	"github.com/apneduniya/logbook/pkg/wire"
)

func openDB(t *testing.T, dir string) *localdb.DB {
	t.Helper()
	db, err := localdb.Open(context.Background(), config.DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// appendSpool appends one event the way hook commands do: open with
// O_APPEND, write one line, close.
func appendSpool(t *testing.T, path string, i int) {
	t.Helper()
	ev := wire.LocalEvent{Type: "ai", AI: &wire.AIEvent{
		Time: time.Now().UTC(), Harness: wire.HarnessClaudeCode, Kind: wire.AIToolEnd, SessionID: fmt.Sprint(i),
	}}
	b, _ := json.Marshal(ev)
	f, err := spoolfile.OpenAppend(path) // how hook commands append
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Error(err)
	}
}

func aiCount(t *testing.T, db *localdb.DB) int64 {
	t.Helper()
	c, err := db.CountSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return c.AIEvents
}

func TestSpoolDrainBasics(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	path := config.SpoolPath(dir)
	sp := NewSpool(path, db, slog.New(slog.DiscardHandler), 0)
	ctx := context.Background()

	if r, err := sp.Drain(ctx); err != nil || r.Stored != 0 {
		t.Fatalf("empty drain = %+v, %v", r, err)
	}
	for i := range 3 {
		appendSpool(t, path, i)
	}
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString("not json\n\n" + `{"type":"git","ai":{}}` + "\n" + strings.Repeat("x", maxSpoolLine+10) + "\n")
	_ = f.Close()
	appendSpool(t, path, 3)

	r, err := sp.Drain(ctx)
	if err != nil || r.Stored != 4 || r.Invalid != 3 {
		t.Fatalf("drain = %+v, %v", r, err)
	}
	if n := aiCount(t, db); n != 4 {
		t.Fatalf("ai events = %d", n)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 0 {
		t.Fatalf("spool dir not empty: %v", entries)
	}
}

func TestSpoolResumesInterruptedDrain(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	path := config.SpoolPath(dir)
	ctx := context.Background()
	for i := range 5 {
		appendSpool(t, path, i)
	}
	// Simulate a crash after claiming the file and storing the first two lines.
	draining := path + ".00000000000000000001" + drainingSuffix
	if err := os.Rename(path, draining); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(draining)
	lines := strings.SplitAfter(string(b), "\n")
	var first []wire.LocalEvent
	for _, l := range lines[:2] {
		var ev wire.LocalEvent
		_ = json.Unmarshal([]byte(l), &ev)
		first = append(first, ev)
	}
	off := len(lines[0]) + len(lines[1])
	mark := localdb.KV{Key: spoolOffsetKey, Value: fmt.Sprintf("%s:%d", filepath.Base(draining), off)}
	if err := db.InsertEvents(ctx, first, mark); err != nil {
		t.Fatal(err)
	}
	appendSpool(t, path, 99) // a new spool appeared meanwhile

	sp := NewSpool(path, db, slog.New(slog.DiscardHandler), 0)
	if r, err := sp.Drain(ctx); err != nil || r.Stored != 4 {
		t.Fatalf("drain = %+v, %v", r, err)
	}
	if n := aiCount(t, db); n != 6 {
		t.Fatalf("ai events = %d, want 6 (no duplicates, nothing lost)", n)
	}
}

func TestSpoolDrainWithConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	path := config.SpoolPath(dir)
	sp := NewSpool(path, db, slog.New(slog.DiscardHandler), 20*time.Millisecond)
	ctx := context.Background()

	const writers, perWriter = 4, 150
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				appendSpool(t, path, w*perWriter+i)
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for drained := false; !drained; {
		select {
		case <-done:
			drained = true
		default:
		}
		if _, err := sp.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sp.Drain(ctx); err != nil { // pick up anything appended after the last claim
		t.Fatal(err)
	}
	if n := aiCount(t, db); n != writers*perWriter {
		t.Fatalf("ai events = %d, want %d", n, writers*perWriter)
	}
}
