package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/config"
)

// runHookWith runs `logbook hook <args>` with stdin from a file and returns
// the exit code and stdout. It swaps os.Stdin/os.Stdout, so it can't run in
// parallel.
func runHookWith(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "stdin")
	if err := os.WriteFile(in, []byte(stdin), 0o600); err != nil {
		t.Fatal(err)
	}
	inF, err := os.Open(in) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inF.Close() }()
	outF, err := os.Create(filepath.Join(dir, "stdout")) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outF.Close() }()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inF, outF
	code := run(context.Background(), append([]string{"hook"}, args...))
	os.Stdin, os.Stdout = oldIn, oldOut
	out, err := os.ReadFile(outF.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

// With no daemon running and no config yet, the hook exits 0, prints the
// harness's protocol response and spools the event.
func TestHookCommandFailsOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOGBOOK_HOME", home)
	t.Setenv("CURSOR_VERSION", "")

	code, out := runHookWith(t, `{"conversation_id":"c1","workspace_roots":["/w/app"],"prompt":"secret prompt"}`, "cursor", "beforeSubmitPrompt")
	if code != 0 || out != `{"continue":true}`+"\n" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	spool, err := os.ReadFile(config.SpoolPath(home))
	if err != nil {
		t.Fatalf("no spool: %v", err)
	}
	if !strings.Contains(string(spool), `"kind":"prompt_submit"`) || strings.Contains(string(spool), "secret prompt") {
		t.Fatalf("spool = %s", spool)
	}
}

// A configured but stopped daemon also falls back to the spool.
func TestHookCommandDaemonDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOGBOOK_HOME", home)
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalAddr = "127.0.0.1:1" // nothing listens on port 1
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	code, out := runHookWith(t, `{"session_id":"s","cwd":"/w/app","model":"gpt"}`, "codex", "SessionStart")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if b, err := os.ReadFile(config.SpoolPath(home)); err != nil || !strings.Contains(string(b), `"kind":"session_start"`) {
		t.Fatalf("spool = %s, %v", b, err)
	}
	if b, _ := os.ReadFile(config.LogPath(home)); strings.Contains(string(b), cfg.LocalToken) {
		t.Fatal("local token written to the log")
	}
}

func TestHookCommandBadArgs(t *testing.T) {
	t.Setenv("LOGBOOK_HOME", t.TempDir())
	for _, args := range [][]string{nil, {"cursor"}, {"nope", "x"}, {"cursor", "stop", "extra"}} {
		code, out := runHookWith(t, `{}`, args...)
		if code != 0 || out != "" {
			t.Fatalf("args %q: exit %d, stdout %q", args, code, out)
		}
	}
}
