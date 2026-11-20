package logfile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w, err := Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 39)
	line = append(line, '\n') // 40 bytes: two lines fit in 100, a third rotates
	for range 7 {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, name := range []string{"agent.log", "agent.log.1", "agent.log.2", "agent.log.3"} {
		fi, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if err == nil {
			sizes[name] = fi.Size()
		}
	}
	want := map[string]int64{"agent.log": 40, "agent.log.1": 80, "agent.log.2": 80}
	if len(sizes) != len(want) {
		t.Fatalf("files = %v, want %v", sizes, want)
	}
	for k, v := range want {
		if sizes[k] != v {
			t.Fatalf("files = %v, want %v", sizes, want)
		}
	}
	if _, err := w.Write(line); err == nil {
		t.Fatal("write after close succeeded")
	}
}

func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	for range 2 {
		w, err := Open(path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("hello\n")); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
	}
	b, _ := os.ReadFile(path)
	if string(b) != "hello\nhello\n" {
		t.Fatalf("content = %q", b)
	}
}
