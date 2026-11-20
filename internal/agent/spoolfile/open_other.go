//go:build !windows

package spoolfile

import "os"

// openAppend relies on POSIX semantics: renaming or unlinking an open file
// is always allowed, and O_APPEND writes are atomic with respect to the
// file's end.
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // path comes from config.SpoolPath
}
