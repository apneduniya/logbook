package main

import (
	"os"
	"strings"
)

// isTerminal reports whether f is an interactive terminal: a character
// device, or on Windows the pipe a Cygwin or MSYS2 terminal (mintty, as in
// Git Bash) gives native programs instead of a console. /dev/null is a
// character device too; reading it gives end of input, which counts as no.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err == nil && st.Mode()&os.ModeCharDevice != 0 {
		return true
	}
	return isPTYPipe(f)
}

// isPTYPipeName reports whether name, a named pipe's file name, is the
// pipe of a Cygwin or MSYS2 pseudo-terminal:
// \{cygwin,msys}-<hex id>-pty<n>-{from,to}-master, optionally under
// \Device\NamedPipe.
func isPTYPipeName(name string) bool {
	name = strings.TrimPrefix(name, `\Device\NamedPipe`)
	t := strings.Split(name, "-")
	if len(t) < 5 || (t[0] != `\msys` && t[0] != `\cygwin`) || t[1] == "" {
		return false
	}
	n, ok := strings.CutPrefix(t[2], "pty")
	if !ok || n == "" || strings.Trim(n, "0123456789") != "" {
		return false
	}
	return (t[3] == "from" || t[3] == "to") && t[4] == "master"
}
