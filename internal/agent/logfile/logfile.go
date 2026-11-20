package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
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

	// rename and now are os.Rename and time.Now, replaceable in tests.
	rename func(oldpath, newpath string) error
	now    func() time.Time

	mu   sync.Mutex
	f    *os.File
	size int64
	// retryAt delays the next rotation attempt after one failed.
	retryAt      time.Time
	rotateFailed bool
}

// rotateRetry is how long the writer waits after a failed rotation before
// trying again, appending to the live file meanwhile.
const rotateRetry = time.Minute

// Open opens (or creates, mode 0600) the log at path for appending.
// maxBytes or keep <= 0 take the defaults.
func Open(path string, maxBytes int64, keep int) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	w := &Writer{path: path, maxBytes: maxBytes, keep: keep, rename: os.Rename, now: time.Now}
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
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes && !w.retryAt.After(w.now()) {
		if err := w.rotate(); err != nil {
			// Keep logging to the live file and try again later; tell the
			// user once per failure streak (the log itself may be the only
			// place they'd look, so write it there).
			w.retryAt = w.now().Add(rotateRetry)
			if w.f == nil {
				return 0, err
			}
			if !w.rotateFailed {
				w.rotateFailed = true
				_, _ = fmt.Fprintf(w.f, "logfile: rotation failed, retrying in %s: %v\n", rotateRetry, err)
			}
		} else {
			w.rotateFailed = false
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("writing log: %w", err)
	}
	return n, nil
}

// rotate shifts name.(k-1) → name.k, name → name.1 and reopens. w.mu must
// be held. Whatever fails, the live file is open again afterwards (w.f is
// nil only if even reopening failed), so one failed rotation never ends
// logging. On Windows a rename fails while another process (an antivirus
// scan, a user tailing the log) holds the file without delete sharing.
func (w *Writer) rotate() (err error) {
	cerr := w.f.Close()
	w.f = nil
	defer func() {
		if oerr := w.open(); oerr != nil {
			err = errors.Join(err, oerr)
		}
	}()
	if cerr != nil {
		return fmt.Errorf("closing log: %w", cerr)
	}
	for i := w.keep - 1; i >= 1; i-- {
		if rerr := w.rename(w.backup(i), w.backup(i+1)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return fmt.Errorf("rotating log: %w", rerr)
		}
	}
	// os.Rename replaces an existing target on every OS (MoveFileEx with
	// MOVEFILE_REPLACE_EXISTING on Windows), so the oldest copy is dropped.
	if rerr := w.rename(w.path, w.backup(1)); rerr != nil {
		return fmt.Errorf("rotating log: %w", rerr)
	}
	return nil
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
