//go:build !windows

package main

import "os"

// isPTYPipe is only true on Windows.
func isPTYPipe(*os.File) bool { return false }
