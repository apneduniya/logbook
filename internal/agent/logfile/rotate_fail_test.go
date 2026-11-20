package logfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailedRotationKeepsLogging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w, err := Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	clock := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }
	locked := true // like Windows while an antivirus scan holds agent.log open
	w.rename = func(oldpath, newpath string) error {
		if locked {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.New("sharing violation")}
		}
		return os.Rename(oldpath, newpath)
	}
	line := append(bytes.Repeat([]byte("x"), 39), '\n')
	for i := range 5 {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("write %d after a failed rotation: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("rotated although rename failed")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "rotation failed"); n != 1 {
		t.Fatalf("rotation warnings = %d, want 1; log = %q", n, b)
	}

	// Once the file is free and the retry delay has passed, rotation resumes.
	locked = false
	clock = clock.Add(rotateRetry)
	if _, err := w.Write(line); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != int64(len(line)) {
		t.Fatalf("live log after recovery: %v, %v", fi, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotation after recovery: %v", err)
	}
}
