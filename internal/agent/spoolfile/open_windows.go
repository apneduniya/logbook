package spoolfile

import (
	"os"

	"golang.org/x/sys/windows"
)

// openAppend opens the file the way os.OpenFile does for O_APPEND (append
// data access without write data access, so every write lands at the end of
// the file), but also shares delete access. Without FILE_SHARE_DELETE,
// Windows refuses to rename a file while any handle to it is open, so the
// daemon couldn't claim the spool while a hook was appending. With it, the
// rename succeeds and the hook's open handle follows the file to its new
// name, as on POSIX.
//
// Mode 0600 has no meaning on Windows; the file inherits the ACL of the
// agent's data directory under %AppData%, which other standard users can't
// read.
func openAppend(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p,
		windows.FILE_APPEND_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, // not inheritable, like os.OpenFile
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
