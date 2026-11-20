package spoolfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func write(t *testing.T, f *os.File, s string) {
	t.Helper()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// The daemon renames the spool while hooks may hold it open. The rename
// must succeed on every OS, and a write through a handle opened before the
// rename must land in the renamed file, after what was already there.
func TestRenameWhileOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool", "events.jsonl")
	a, err := OpenAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := OpenAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	write(t, a, "a1\n")
	write(t, b, "b1\n")
	write(t, a, "a2\n")

	claimed := path + ".1.draining"
	if err := os.Rename(path, claimed); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
	write(t, b, "b2\n") // a hook that opened just before the rename

	fresh, err := OpenAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, fresh, "c1\n")
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	if got := read(t, claimed); got != "a1\nb1\na2\nb2\n" {
		t.Fatalf("claimed file = %q", got)
	}
	if got := read(t, path); got != "c1\n" {
		t.Fatalf("new spool = %q", got)
	}
	if err := os.Remove(claimed); err != nil {
		t.Fatalf("removing the claimed file while a hook holds it: %v", err)
	}
}

func TestStatAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool", "events.jsonl")
	f, err := OpenAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	write(t, f, "x\n")
	st, err := f.Stat()
	if err != nil || st.Size() != 2 {
		t.Fatalf("Stat = %v, %v; want size 2", st, err)
	}
	if runtime.GOOS == "windows" {
		return // modes are advisory there
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", st.Mode().Perm())
	}
	if di, err := os.Stat(filepath.Dir(path)); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v; want 0700", di.Mode().Perm(), err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
