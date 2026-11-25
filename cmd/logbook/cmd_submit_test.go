package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/config"
)

func TestParseSubmit(t *testing.T) {
	for _, args := range [][]string{
		{"terra bot", "--from", "2026-11-01", "--to", "2026-11-20", "--program", "terra", "--yes"},
		{"--from", "2026-11-01", "--to=2026-11-20", "terra bot", "--yes", "--program", "terra"},
		{"--from", "2026-11-01", "--to", "2026-11-20", "--program", "terra", "--yes", "terra bot"},
	} {
		req, err := parseSubmit(args)
		if err != nil || req.Project != "terra bot" || req.From != "2026-11-01" || req.To != "2026-11-20" ||
			req.Program != "terra" || !req.Yes || req.Preview != "" || req.AddTo != "" {
			t.Errorf("%q: %+v, %v", args, req, err)
		}
	}
	for _, bad := range [][]string{
		{"--from", "2026-11-01", "--to", "2026-11-20"},
		{"a", "b", "--from", "2026-11-01", "--to", "2026-11-20"},
		{"a", "--from", "2026-11-01"},
		{"a", "--from", "2026-11-01", "--to", "2026-11-20", "--nope"},
	} {
		if _, err := parseSubmit(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// submit needs a signed-in, registered agent before it asks the server
// anything.
func TestSubmitNeedsSetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOGBOOK_HOME", home)
	ctx := context.Background()
	args := []string{"app", "--from", "2026-11-01", "--to", "2026-11-02", "--yes"}
	var out bytes.Buffer
	if err := runSubmit(ctx, args, nil, &out, false); err == nil || !strings.Contains(err.Error(), "logbook login") {
		t.Fatalf("not signed in: %v", err)
	}
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIKey = coreTestKey
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := runSubmit(ctx, args, nil, &out, false); err == nil || !strings.Contains(err.Error(), "logbook run") {
		t.Fatalf("no local log: %v", err)
	}
	if _, err := os.Stat(config.DBPath(home)); err == nil {
		t.Fatal("submit created a database")
	}
}

// verify reads a bundle from any path, gzip-compressed or not whatever
// its name, and exits non-zero when a check fails.
func TestVerifyCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit files")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A bundle with nothing in it checks: no rows, no cap, nothing to fail.
	empty := `{"format":"logbook-submission-bundle-v1","submission":{"id":"c0000000-0000-4000-8000-000000000003",` +
		`"project":"app","from":"2026-11-20","to":"2026-11-20","created_at":"2026-11-22T00:00:00Z"}}`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(empty))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "BUNDLE.JSON.GZ")
	bad := filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(good, gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(strings.Replace(empty, "-v1", "-v0", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runVerify([]string{good}, &out); err != nil || !strings.Contains(out.String(), "Result                  OK") {
		t.Fatalf("good bundle: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runVerify([]string{bad}, &out); !errors.Is(err, errVerifyFailed) || !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("bad bundle: %v\n%s", err, out.String())
	}
	// --key pins the devices' keys, before or after the file; the empty
	// bundle has no device, so any fingerprint passes.
	for _, args := range [][]string{{"--key", "66687aadf862bd77", good}, {good, "--key", "66687aadf862bd77"}} {
		out.Reset()
		if err := runVerify(args, &out); err != nil || !strings.Contains(out.String(), "every key, signature") {
			t.Fatalf("%q: %v\n%s", args, err, out.String())
		}
	}
	for _, args := range [][]string{nil, {"a", "b"}, {"--x"}, {"--key"}, {good, "--key"}, {filepath.Join(dir, "missing.json")}} {
		if err := runVerify(args, &out); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
}
