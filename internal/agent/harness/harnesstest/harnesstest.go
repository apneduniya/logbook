package harnesstest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/internal/agent/harness/jsonedit"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Bin and OtherBin are the absolute binary paths the install tests use.
const (
	Bin      = "/opt/logbook/bin/logbook"
	OtherBin = "/home/teen/.local/bin/logbook"
)

// Sentinel is planted in every fixture field Logbook must never keep
// (prompts, tool input and output, transcripts, messages).
const Sentinel = "SENTINEL-PRIVATE"

// Installer is the installer shape under test.
type Installer interface {
	Name() string
	Install(binPath string) error
	Uninstall() error
	Installed() (bool, error)
}

// Env returns a test Env rooted at a fresh temp home, and its backup dir.
func Env(t *testing.T) (hostenv.Env, string) {
	t.Helper()
	backups := filepath.Join(t.TempDir(), "backups")
	return hostenv.ForTest(t.TempDir(), "linux", backups), backups
}

// Golden compares got with testdata/<name>.golden, rewriting the file when
// LOGBOOK_UPDATE_GOLDEN=1.
func Golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("LOGBOOK_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("reading golden (set LOGBOOK_UPDATE_GOLDEN=1 to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// Fixture reads testdata/<name>.
func Fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// InstallCase is one starting state for a JSON config install test.
type InstallCase struct {
	// Name names the golden file: testdata/install_<Name>.golden.
	Name string
	// Existing is the config file's content before install ("" = no file).
	Existing string
}

// JSONInstallRoundTrip checks, for each case, that:
//   - Install writes exactly the golden file;
//   - Installed reports true, and re-installing changes nothing and adds no
//     backup;
//   - installing with another binary path replaces Logbook's entries
//     without duplicating them;
//   - Uninstall restores the original semantically (or removes a file
//     Logbook created), Installed then reports false, and a second
//     Uninstall is a no-op.
func JSONInstallRoundTrip(t *testing.T, newInstaller func(hostenv.Env) Installer, configPath func(hostenv.Env) (string, error), jsonc bool, cases []InstallCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			env, backups := Env(t)
			in := newInstaller(env)
			path, err := configPath(env)
			if err != nil {
				t.Fatal(err)
			}
			if tc.Existing != "" {
				WriteFile(t, path, tc.Existing)
			}
			if ok, err := in.Installed(); err != nil || ok {
				t.Fatalf("Installed() before install = %v, %v; want false, nil", ok, err)
			}

			if err := in.Install(Bin); err != nil {
				t.Fatalf("Install: %v", err)
			}
			first := ReadFile(t, path)
			Golden(t, "install_"+tc.Name, first)
			if ok, err := in.Installed(); err != nil || !ok {
				t.Fatalf("Installed() after install = %v, %v; want true, nil", ok, err)
			}
			backupsAfterFirst := listDir(t, backups)
			if len(backupsAfterFirst) != 1 {
				t.Fatalf("backups after first install = %v, want exactly one", backupsAfterFirst)
			}
			if tc.Existing != "" {
				orig := ReadFile(t, filepath.Join(backups, backupsAfterFirst[0]))
				if string(orig) != tc.Existing {
					t.Fatalf("backup content = %q, want the original file", orig)
				}
			}

			if err := in.Install(Bin); err != nil {
				t.Fatalf("second Install: %v", err)
			}
			if again := ReadFile(t, path); !bytes.Equal(again, first) {
				t.Fatalf("re-install changed the file:\n%s", again)
			}
			if got := listDir(t, backups); !reflect.DeepEqual(got, backupsAfterFirst) {
				t.Fatalf("re-install changed backups: %v", got)
			}

			if err := in.Install(OtherBin); err != nil {
				t.Fatalf("Install(OtherBin): %v", err)
			}
			moved := ReadFile(t, path)
			if bytes.Contains(moved, []byte(Bin)) || bytes.Count(moved, []byte(OtherBin)) != bytes.Count(first, []byte(Bin)) {
				t.Fatalf("install with a new binary path didn't replace entries one for one:\n%s", moved)
			}

			if err := in.Uninstall(); err != nil {
				t.Fatalf("Uninstall: %v", err)
			}
			if ok, err := in.Installed(); err != nil || ok {
				t.Fatalf("Installed() after uninstall = %v, %v; want false, nil", ok, err)
			}
			after, existed := readMaybe(t, path)
			switch {
			case tc.Existing == "" && existed:
				t.Fatalf("uninstall left a file Logbook created:\n%s", after)
			case tc.Existing != "" && !existed:
				t.Fatal("uninstall removed the user's file")
			case tc.Existing != "":
				SameJSON(t, after, []byte(tc.Existing), jsonc)
			}
			if err := in.Uninstall(); err != nil {
				t.Fatalf("second Uninstall: %v", err)
			}
		})
	}
}

// SameJSON fails unless got and want decode to equal values.
func SameJSON(t *testing.T, got, want []byte, jsonc bool) {
	t.Helper()
	if jsonc {
		want, _ = jsonedit.StripJSONC(want)
		got, _ = jsonedit.StripJSONC(got) // comments are kept on rewrite
	}
	if len(bytes.TrimSpace(want)) == 0 {
		want = []byte("{}") // a blank config file is an empty object
	}
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("result isn't JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("expected isn't JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("not semantically equal\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// ParseFunc is a harness parser.
type ParseFunc func(event string, in aievent.Input, getenv func(string) string) []aievent.Fields

// ParseFixture parses testdata/<fixture> as event, builds the wire events and
// checks none of them contains Sentinel.
func ParseFixture(t *testing.T, h wire.Harness, parse ParseFunc, event, fixture string) []wire.AIEvent {
	t.Helper()
	in := aievent.Input{Data: Fixture(t, fixture)}
	return Build(t, h, parse(event, in, func(string) string { return "" }))
}

// Now is the fixed clock parser tests use.
func Now() time.Time { return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC) }

// Build turns fields into validated wire events, failing on any leak.
func Build(t *testing.T, h wire.Harness, fields []aievent.Fields) []wire.AIEvent {
	t.Helper()
	project := func(cwd string) string { return filepath.Base(cwd) }
	out := make([]wire.AIEvent, 0, len(fields))
	for _, f := range fields {
		ev := aievent.Build(h, f, Now(), project)
		if err := ev.Validate(); err != nil {
			t.Fatalf("event invalid: %v (%+v)", err, ev)
		}
		NoLeak(t, ev)
		out = append(out, ev)
	}
	return out
}

// NoLeak fails if v's JSON encoding contains Sentinel.
func NoLeak(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), Sentinel) {
		t.Fatalf("private text leaked into event: %s", b)
	}
}

// BigPayload returns a JSON object whose leading fields are kept, followed
// by a field holding more than aievent.MaxPayload bytes of private text, the
// shape of a hook payload carrying a huge tool output.
func BigPayload(lead string) []byte {
	var b bytes.Buffer
	b.WriteString("{" + lead + `,"tool_response":"`)
	chunk := strings.Repeat(Sentinel+" ", 64)
	for b.Len() < aievent.MaxPayload+4096 {
		b.WriteString(chunk)
	}
	b.WriteString(`"}`)
	return b.Bytes()
}

// ReadFile reads path or fails the test.
func ReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readMaybe(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test path
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// WriteFile creates path (and its directory) with content.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
