package localpost

import "runtime"

// runtimeNotWindows reports whether Unix permission bits are meaningful.
func runtimeNotWindows() bool { return runtime.GOOS != "windows" }
