package daemon

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
)

// A program that opens the spool without delete sharing (os.OpenFile does
// so) blocks the rename. A short hold is waited out; a long one fails the
// claim without losing anything, and the next drain gets the events.
func TestSpoolClaimWaitsOutSharingViolation(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	path := config.SpoolPath(dir)
	appendSpool(t, path, 1)
	sp := NewSpool(path, db, slog.New(slog.DiscardHandler), 0)
	ctx := context.Background()

	hold := func() *os.File {
		f, err := os.Open(path) //nolint:gosec // test path
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	f := hold()
	if _, err := sp.Drain(ctx); !isSharingViolation(err) {
		_ = f.Close()
		t.Fatalf("Drain while held = %v, want a sharing violation", err)
	}
	_ = f.Close()

	f = hold()
	time.AfterFunc(15*time.Millisecond, func() { _ = f.Close() })
	if r, err := sp.Drain(ctx); err != nil || r.Stored != 1 {
		t.Fatalf("Drain after a short hold = %+v, %v; want 1 stored", r, err)
	}
}
