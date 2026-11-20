package spoolfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// OpenAppend opens path for appending, creating it (mode 0600) and its
// directory (mode 0700) if needed. Every write goes to the current end of
// the file, so concurrent appenders never overwrite each other, and the
// file can be renamed or deleted while it is open: on Windows it is opened
// with FILE_SHARE_DELETE, which os.OpenFile doesn't set.
func OpenAppend(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // inside the agent's data dir
		return nil, fmt.Errorf("creating spool dir: %w", err)
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, fmt.Errorf("opening spool: %w", err)
	}
	return f, nil
}
