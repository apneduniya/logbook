package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/config"
)

func TestLoginWarnsWhenEditorsUseAnotherKey(t *testing.T) {
	agentURL := "http://" + config.DefaultLocalAddr + "/api/v1"
	tests := []struct {
		name     string
		cfg      string // .wakatime.cfg content; empty means no file
		wantHint bool
	}{
		{"old key at the agent", "[settings]\napi_url = " + agentURL + "\napi_key = waka_old\n", true},
		{"current key", "[settings]\napi_url = " + agentURL + "\napi_key = " + coreTestKey + "\n", false},
		{"not routed through the agent", "[settings]\napi_url = https://hackatime.hackclub.com/api/hackatime/v1\napi_key = x\n", false},
		{"no wakatime.cfg", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LOGBOOK_HOME", t.TempDir())
			waka := t.TempDir()
			t.Setenv("WAKATIME_HOME", waka)
			if tt.cfg != "" {
				if err := os.WriteFile(filepath.Join(waka, ".wakatime.cfg"), []byte(tt.cfg), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			srv := fakeLogbook(t)
			var out bytes.Buffer
			if err := login(context.Background(), []string{"--key", coreTestKey, "--server", srv.URL}, nil, &out); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(out.String(), "logbook install"); got != tt.wantHint {
				t.Fatalf("hint shown = %v, want %v; output %q", got, tt.wantHint, out.String())
			}
			if strings.Contains(out.String(), coreTestKey) || strings.Contains(out.String(), "waka_old") {
				t.Fatal("login printed a key")
			}
		})
	}
}
