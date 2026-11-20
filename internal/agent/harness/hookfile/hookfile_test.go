package hookfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/harness/cmdline"
)

const bin = "/opt/logbook/bin/logbook"

// testSpec is a minimal Flat-layout harness writing to dir/conf/hooks.json.
func testSpec(dir string) Spec {
	return Spec{
		Harness: "codex",
		Path:    func() (string, error) { return filepath.Join(dir, "conf", "hooks.json"), nil },
		Layout:  Flat,
		Header:  []Field{{Key: "version", Value: json.RawMessage("1")}},
		Entries: func(b string) []Entry {
			h, _ := json.Marshal(map[string]string{"command": cmdline.Quote(cmdline.POSIX, cmdline.Hook(b, "codex", "Stop"))})
			return []Entry{{Event: "Stop", Handler: h}}
		},
		Argv:   CommandArgv,
		Detect: func() bool { return true },
		BackupDir: func() (string, error) {
			return filepath.Join(dir, "backups"), os.MkdirAll(filepath.Join(dir, "backups"), 0o700)
		},
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "hooks.json")
	write(t, target, `{"version":1,"hooks":{}}`, 0o644)
	link := filepath.Join(dir, "conf", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	in := New(testSpec(dir))
	if err := in.Install(bin); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a file")
	}
	b, _ := os.ReadFile(target) //nolint:gosec // test path
	if !strings.Contains(string(b), "hook codex Stop") {
		t.Fatalf("target not updated: %s", b)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %v", fi.Mode())
	}
}

func TestNewFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	dir := t.TempDir()
	if err := New(testSpec(dir)).Install(bin); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "conf", "hooks.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode %v, %v", fi.Mode(), err)
	}
}

func TestTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf", "hooks.json")
	write(t, path, `{"x":"`+strings.Repeat("a", maxConfigSize)+`"}`, 0o600)
	if err := New(testSpec(dir)).Install(bin); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Install = %v, want ErrTooLarge", err)
	}
}

func TestNotRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conf", "hooks.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := New(testSpec(dir)).Install(bin); err == nil {
		t.Fatal("Install into a directory succeeded")
	}
}

// The first backup is kept across installs, so it always holds the user's
// original file.
func TestBackupKeepsFirstOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf", "hooks.json")
	write(t, path, `{"version":1,"hooks":{"stop":[{"command":"./mine.sh"}]}}`, 0o600)
	in := New(testSpec(dir))
	if err := in.Install(bin); err != nil {
		t.Fatal(err)
	}
	if err := in.Install("/elsewhere/logbook"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "backups", "codex.hooks.json.orig")) //nolint:gosec // test path
	if err != nil || string(b) != `{"version":1,"hooks":{"stop":[{"command":"./mine.sh"}]}}` {
		t.Fatalf("backup = %q, %v", b, err)
	}
}

// Without a backup (data dir wiped), uninstall still removes only Logbook's
// entries and keeps the header.
func TestUninstallWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf", "hooks.json")
	write(t, path, `{"version":1,"hooks":{"Stop":[{"command":"/opt/logbook/bin/logbook hook codex Stop"},{"command":"./mine.sh"}]}}`, 0o600)
	if err := New(testSpec(dir)).Uninstall(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path) //nolint:gosec // test path
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != float64(1) || !strings.Contains(string(b), "mine.sh") || strings.Contains(string(b), "logbook") {
		t.Fatalf("after uninstall: %s", b)
	}
}

func TestCommandArgv(t *testing.T) {
	tests := map[string][]string{
		`{"type":"command","command":"/x/logbook","args":["hook","claude-code","Stop"]}`: {"/x/logbook", "hook", "claude-code", "Stop"},
		`{"command":"'/x y/logbook' hook cursor stop"}`:                                  {"/x y/logbook", "hook", "cursor", "stop"},
		`{"type":"http","url":"http://x"}`:                                               nil,
		`{"type":"command","command":""}`:                                                nil,
		`"not an object"`:                                                                nil,
	}
	for in, want := range tests {
		got := CommandArgv(json.RawMessage(in))
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("CommandArgv(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestCopilotArgv(t *testing.T) {
	tests := map[string]string{
		`{"type":"command","exec":"/x/logbook","args":["hook","copilot","agentStop"]}`: "/x/logbook|hook|copilot|agentStop",
		`{"bash":"/x/logbook hook copilot agentStop"}`:                                 "/x/logbook|hook|copilot|agentStop",
		`{"powershell":"& 'C:\\a b\\logbook.exe' hook copilot agentStop"}`:             `C:\a b\logbook.exe|hook|copilot|agentStop`,
		`{"type":"prompt","prompt":"x"}`:                                               "",
	}
	for in, want := range tests {
		if got := strings.Join(CopilotArgv(json.RawMessage(in)), "|"); got != want {
			t.Errorf("CopilotArgv(%s) = %q, want %q", in, got, want)
		}
	}
}
