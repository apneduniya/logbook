//go:build !windows

package daemon

// isSharingViolation is always false: POSIX systems rename open files.
func isSharingViolation(error) bool { return false }
