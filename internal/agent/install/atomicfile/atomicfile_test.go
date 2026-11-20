package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "f.txt")
	if err := Write(p, []byte("one"), 0o600); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := Write(p, []byte("two"), 0o600); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	b, ok, err := ReadOptional(p)
	if err != nil || !ok || string(b) != "two" {
		t.Fatalf("ReadOptional() = %q, %v, %v", b, ok, err)
	}
	if runtime.GOOS != "windows" {
		m, err := Mode(p, 0o644)
		if err != nil || m != 0o600 {
			t.Fatalf("Mode() = %v, %v; want 0600", m, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestOptionalHelpers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing")
	if _, ok, err := ReadOptional(p); ok || err != nil {
		t.Fatalf("ReadOptional(missing) = %v, %v", ok, err)
	}
	if m, err := Mode(p, 0o640); err != nil || m != 0o640 {
		t.Fatalf("Mode(missing) = %v, %v", m, err)
	}
	if removed, err := RemoveOptional(p); removed || err != nil {
		t.Fatalf("RemoveOptional(missing) = %v, %v", removed, err)
	}
}
