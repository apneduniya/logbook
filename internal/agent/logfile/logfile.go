package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Defaults: 5 MiB per file, three old files, so at most 20 MiB on disk.
const (
	DefaultMaxBytes = 5 << 20
	DefaultKeep     = 3
)

// Writer is an io.WriteCloser that rotates by size. It is safe for
// concurrent use.
type Writer struct {
	path     string
	maxBytes int64
	keep     int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens (or creates, mode 0600) the log at path for appending.
// maxBytes or keep <= 0 take the defaults.
func Open(path string, maxBytes int64, keep int) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	w := &Writer{path: path, maxBytes: maxBytes, keep: keep}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating log dir: %w", err)
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("reading log size: %w", err)
	}
	w.f, w.size = f, fi.Size()
	return nil
}

// Write appends p, rotating first if p would push the file past MaxBytes.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, errors.New("log file is closed")
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("writing log: %w", err)
	}
	return n, nil
}

// rotate shifts name.(k-1) → name.k, name → name.1 and reopens. w.mu must be held.
func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("closing log: %w", err)
	}
	w.f = nil
	for i := w.keep - 1; i >= 1; i-- {
		err := os.Rename(w.backup(i), w.backup(i+1))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("rotating log: %w", err)
		}
	}
	// os.Rename replaces an existing target on every OS (MoveFileEx with
	// MOVEFILE_REPLACE_EXISTING on Windows), so the oldest copy is dropped.
	if err := os.Rename(w.path, w.backup(1)); err != nil {
		return fmt.Errorf("rotating log: %w", err)
	}
	return w.open()
}

func (w *Writer) backup(i int) string { return fmt.Sprintf("%s.%d", w.path, i) }

// Close closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	if err != nil {
		return fmt.Errorf("closing log: %w", err)
	}
	return nil
}
