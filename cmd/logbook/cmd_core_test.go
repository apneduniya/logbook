package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/config"
)

const coreTestKey = "waka_00000000-0000-4000-8000-0000000000c1"

func fakeLogbook(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(coreTestKey)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`)) // the shape of the real statusbar reply
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOGBOOK_HOME", home)
	srv := fakeLogbook(t)
	ctx := context.Background()
	var out bytes.Buffer

	if err := login(ctx, []string{"--key", "waka_wrong", "--server", srv.URL}, nil, &out); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err := config.Load(home); !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("config saved after a rejected key: %v", err)
	}
	if err := login(ctx, []string{"--key", "-", "--server", srv.URL + "/"}, strings.NewReader(coreTestKey+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil || cfg.APIKey != coreTestKey || cfg.ServerURL != srv.URL || cfg.LocalToken == "" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if strings.Contains(out.String(), coreTestKey) {
		t.Fatal("login printed the key")
	}
	// Logging in again keeps the rest of the config (e.g. the local token).
	if err := login(ctx, []string{"--key", coreTestKey}, nil, &out); err != nil {
		t.Fatal(err)
	}
	cfg2, _ := config.Load(home)
	if cfg2.LocalToken != cfg.LocalToken || cfg2.ServerURL != srv.URL {
		t.Fatalf("relogin changed config: %+v", cfg2)
	}
	for _, bad := range [][]string{{}, {"--key", "a b"}, {"--key", coreTestKey, "--server", "ftp://x"}} {
		if err := login(ctx, bad, nil, &out); err == nil {
			t.Errorf("login %v succeeded", bad)
		}
	}
}

func TestStatusAndDoctorWithDaemonDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOGBOOK_HOME", home)
	t.Setenv("WAKATIME_HOME", home)
	srv := fakeLogbook(t)
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL, cfg.APIKey, cfg.LocalAddr = srv.URL, coreTestKey, "127.0.0.1:1"
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out bytes.Buffer
	if err := showStatus(ctx, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not running") {
		t.Fatalf("status = %s", out.String())
	}
	if _, err := os.Stat(config.DBPath(home)); err == nil {
		t.Fatal("status created a database")
	}

	wcfg := "[settings]\napi_url = http://127.0.0.1:1/api/v1\napi_key = " + coreTestKey + "\n"
	if err := os.WriteFile(filepath.Join(home, ".wakatime.cfg"), []byte(wcfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = runDoctor(ctx, nil, &out)
	if !errors.Is(err, errProblemsFound) {
		t.Fatalf("doctor err = %v", err)
	}
	report := out.String()
	for _, want := range []string{"[FAIL] agent", "[OK  ] api key", "[OK  ] wakatime.cfg", "logbook run"} {
		if !strings.Contains(report, want) {
			t.Errorf("doctor report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, coreTestKey) || strings.Contains(report, cfg.LocalToken) {
		t.Fatal("doctor printed a secret")
	}
}
