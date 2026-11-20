package main

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestInstallFlagErrors(t *testing.T) {
	t.Setenv(keyEnv, "")
	tests := []struct {
		name string
		run  func(context.Context, []string) error
		args []string
		want string
	}{
		{"install needs a key", runInstall, nil, "--key is required"},
		{"install rejects extra args", runInstall, []string{"--key", "k", "extra"}, "unexpected argument"},
		{"install rejects unknown flags", runInstall, []string{"--nope"}, "flag provided but not defined"},
		{"uninstall rejects extra args", runUninstall, []string{"extra"}, "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(context.Background(), tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCommandsRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, c := range commands() {
		names[c.Name] = true
	}
	for _, n := range []string{"install", "uninstall"} {
		if !names[n] {
			t.Errorf("command %q not registered", n)
		}
	}
}

func TestInstallNeedsDaemonCommand(t *testing.T) {
	if hasCommand(commands(), daemonCommand) {
		t.Skip("this build has the daemon command")
	}
	err := runInstall(context.Background(), []string{"--key", "k"})
	if !errors.Is(err, errNoDaemon) {
		t.Fatalf("err = %v, want errNoDaemon", err)
	}
}

func TestPathHint(t *testing.T) {
	winBin := `C:\Users\Ada\AppData\Local\Programs\Logbook\logbook.exe`
	tests := []struct {
		name, goos, bin, path, want string
	}{
		{"linux on PATH", "linux", "/home/u/.local/bin/logbook", "/usr/bin:/home/u/.local/bin/", ""},
		{"linux not on PATH", "linux", "/home/u/.local/bin/logbook", "/usr/bin", `add "/home/u/.local/bin" to your PATH`},
		{"windows prints the path and uninstall", "windows", winBin, "", `& "` + winBin + `" uninstall`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.goos != "windows" && runtime.GOOS == "windows" {
				t.Skip("POSIX paths aren't absolute on Windows, and this branch never runs there")
			}
			got := pathHint(tt.goos, tt.bin, tt.path)
			if (tt.want == "" && got != "") || !strings.Contains(got, tt.want) {
				t.Fatalf("pathHint() = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}
