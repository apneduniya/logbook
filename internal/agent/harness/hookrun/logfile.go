package hookrun

import (
	"fmt"
	"os"
	"sync"
)

// LazyFile is an io.Writer that appends to a file, opening it on the first
// write. Hooks almost never log, so most runs never touch the file. Each
// Write is one append, so lines from concurrent hooks don't interleave.
type LazyFile struct {
	path func() (string, error)
	mu   sync.Mutex
	f    *os.File
	err  error
}

// NewLazyFile returns a writer for the file path returns.
func NewLazyFile(path func() (string, error)) *LazyFile { return &LazyFile{path: path} }

// Write appends p.
func (l *LazyFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil && l.err == nil {
		path, err := l.path()
		if err == nil {
			l.f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // the agent's own log file
		}
		if err != nil {
			l.err = fmt.Errorf("opening hook log: %w", err)
		}
	}
	if l.err != nil {
		return 0, l.err
	}
	n, err := l.f.Write(p)
	if err != nil {
		return n, fmt.Errorf("writing hook log: %w", err)
	}
	return n, nil
}

// Close closes the file if it was opened.
func (l *LazyFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("closing hook log: %w", err)
	}
	return nil
}
