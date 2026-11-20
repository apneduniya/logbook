package main

import (
	"context"
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
