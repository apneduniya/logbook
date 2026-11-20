package daemon

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isSharingViolation reports whether err means another process has the file
// open without the sharing a rename needs.
func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
