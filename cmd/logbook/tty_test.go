package main

import "testing"

// Git Bash's mintty and other Cygwin or MSYS2 terminals give native
// programs a named pipe, not a console; its name says it is a terminal, so
// `logbook submit` can still ask before uploading.
func TestIsPTYPipeName(t *testing.T) {
	for name, want := range map[string]bool{
		`\msys-1888ae32e00d56aa-pty0-from-master`:                   true,
		`\cygwin-e022582115c10879-pty4-to-master`:                   true,
		`\Device\NamedPipe\msys-1888ae32e00d56aa-pty12-from-master`: true,
		`\msys-1888ae32e00d56aa-pty0-from-master-nat`:               true,
		`\msys-1888ae32e00d56aa-pty0-to-slave`:                      false,
		`\msys--pty0-from-master`:                                   false,
		`\msys-1888ae32e00d56aa-ptyx-from-master`:                   false,
		`\msys-1888ae32e00d56aa-pty-from-master`:                    false,
		`\mintty-1888ae32e00d56aa-pty0-from-master`:                 false,
		`\msys-1888ae32e00d56aa-pty0`:                               false,
		`\pipe\something`:                                           false,
		"":                                                          false,
	} {
		if got := isPTYPipeName(name); got != want {
			t.Errorf("isPTYPipeName(%q) = %v, want %v", name, got, want)
		}
	}
}
