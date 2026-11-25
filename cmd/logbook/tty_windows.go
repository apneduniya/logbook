//go:build windows

package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isPTYPipe reports whether f is the named pipe of a Cygwin or MSYS2
// pseudo-terminal (isPTYPipeName).
func isPTYPipe(f *os.File) bool {
	h := windows.Handle(f.Fd())
	if t, err := windows.GetFileType(h); err != nil || t != windows.FILE_TYPE_PIPE {
		return false
	}
	// FILE_NAME_INFO: a uint32 length in bytes, then the UTF-16 name.
	var buf [4 + windows.MAX_PATH*2]byte
	if err := windows.GetFileInformationByHandleEx(h, windows.FileNameInfo, &buf[0], uint32(len(buf))); err != nil {
		return false
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	if n == 0 || n > uint32(len(buf)-4) || n%2 != 0 {
		return false
	}
	name := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[4])), n/2)
	return isPTYPipeName(windows.UTF16ToString(name))
}
