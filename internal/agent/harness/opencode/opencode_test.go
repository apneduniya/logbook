package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/harnesstest"
	"github.com/apneduniya/logbook/pkg/wire"
)

func TestInstallRoundTrip(t *testing.T) {
	env, _ := harnesstest.Env(t)
	in := NewInstaller(env)
	path, err := PluginPath(env)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(path), "someone-elses.js")
	harnesstest.WriteFile(t, other, "export default {}\n")

	if ok, err := in.Installed(); err != nil || ok {
		t.Fatalf("Installed() = %v, %v before install", ok, err)
	}
	if err := in.Install(harnesstest.Bin); err != nil {
		t.Fatal(err)
	}
	first := harnesstest.ReadFile(t, path)
	harnesstest.Golden(t, "install_plugin", first)
	if ok, err := in.Installed(); err != nil || !ok {
		t.Fatalf("Installed() = %v, %v after install", ok, err)
	}
	st1, _ := os.Stat(path)
	if err := in.Install(harnesstest.Bin); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(path)
	if !st1.ModTime().Equal(st2.ModTime()) || string(harnesstest.ReadFile(t, path)) != string(first) {
		t.Fatal("re-install rewrote the plugin")
	}
	if err := in.Install(harnesstest.OtherBin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(harnesstest.ReadFile(t, path)), `const BIN = "`+harnesstest.OtherBin+`"`) {
		t.Fatal("install with a new path didn't update the plugin")
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin still there after uninstall: %v", err)
	}
	if string(harnesstest.ReadFile(t, other)) != "export default {}\n" {
		t.Fatal("another plugin was touched")
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallLeavesForeignFile(t *testing.T) {
	env, _ := harnesstest.Env(t)
	path, _ := PluginPath(env)
	harnesstest.WriteFile(t, path, "// my own logbook plugin\n")
	in := NewInstaller(env)
	if err := in.Install(harnesstest.Bin); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("Install over a foreign file = %v, want ErrNotOurs", err)
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if string(harnesstest.ReadFile(t, path)) != "// my own logbook plugin\n" {
		t.Fatal("foreign file changed")
	}
	if ok, _ := in.Installed(); ok {
		t.Fatal("foreign file reported as installed")
	}
}

func TestConfigDirHonoursXDG(t *testing.T) {
	env, _ := harnesstest.Env(t)
	env.Getenv = func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/xdg"
		}
		return ""
	}
	if got, err := PluginPath(env); err != nil || got != "/xdg/opencode/plugins/logbook.js" {
		t.Fatalf("PluginPath = %q, %v", got, err)
	}
}

func TestRenderEscapesPath(t *testing.T) {
	src := string(renderPlugin(`C:\Users\O"Brien\logbook.exe`))
	if !strings.Contains(src, `const BIN = "C:\\Users\\O\"Brien\\logbook.exe"`) {
		t.Fatalf("binary path not JSON-escaped:\n%s", src[:400])
	}
}

func TestParse(t *testing.T) {
	base := wire.AIEvent{Time: harnesstest.Now(), Harness: wire.HarnessOpenCode, SessionID: "ses_01",
		Cwd: "/home/teen/my-app", Project: "my-app", Model: "claude-sonnet-5-5"}
	with := func(f func(*wire.AIEvent)) wire.AIEvent { e := base; f(&e); return e }
	tests := []struct {
		event string
		want  wire.AIEvent
	}{
		{"session-start", with(func(e *wire.AIEvent) { e.Kind = wire.AISessionStart })},
		{"prompt", with(func(e *wire.AIEvent) { e.Kind, e.PromptChars = wire.AIPromptSubmit, 42 })},
		{"tool-start", with(func(e *wire.AIEvent) { e.Kind, e.Tool = wire.AIToolStart, "bash" })},
		{"tool-end", with(func(e *wire.AIEvent) { e.Kind, e.Tool = wire.AIToolEnd, "bash" })},
		{"permission-ask", with(func(e *wire.AIEvent) { e.Kind, e.Model = wire.AIPermissionAsk, "" })},
		{"permission-reply", with(func(e *wire.AIEvent) { e.Kind, e.Model, e.Decision = wire.AIPermissionAsk, "", "deny" })},
		{"turn-end", with(func(e *wire.AIEvent) { e.Kind = wire.AITurnEnd })},
		{"session-end", with(func(e *wire.AIEvent) { e.Kind = wire.AISessionEnd })},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			got := harnesstest.ParseFixture(t, wire.HarnessOpenCode, Parse, tc.event, tc.event+".json")
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestParseIgnoresBadDecision(t *testing.T) {
	in := aievent.Input{Data: []byte(`{"session_id":"s","decision":"maybe"}`)}
	got := harnesstest.Build(t, wire.HarnessOpenCode, Parse("permission-reply", in, nil))
	if len(got) != 1 || got[0].Decision != "" {
		t.Fatalf("got %+v", got)
	}
}

// TestPluginInNode runs the real plugin under Node with fake OpenCode V2 and
// V1 contexts and a fake logbook binary that records its args and stdin. It
// checks the plugin sends the right events and never sends private text.
func TestPluginInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs node and a POSIX shell")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "calls.log")
	fake := filepath.Join(dir, "logbook")
	script := "#!/bin/sh\nprintf '%s %s %s\\t%s\\n' \"$1\" \"$2\" \"$3\" \"$(cat)\" >> '" + out + "'\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	plugin := filepath.Join(dir, "logbook.mjs")
	if err := os.WriteFile(plugin, renderPlugin(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	driver, err := filepath.Abs(filepath.Join("testdata", "driver.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, driver, plugin, harnesstest.Sentinel) //nolint:gosec // test runs its own fixture
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node driver: %v\n%s", err, b)
	}
	raw := string(harnesstest.ReadFile(t, out))
	if strings.Contains(raw, harnesstest.Sentinel) {
		t.Fatalf("private text reached the hook binary:\n%s", raw)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		args, payload, _ := strings.Cut(line, "\t")
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		keys := make([]string, 0, len(m))
		for k, v := range m {
			if v != "" && v != float64(0) {
				keys = append(keys, k+"="+toString(v))
			}
		}
		sort.Strings(keys)
		calls = append(calls, args+" "+strings.Join(keys, ","))
	}
	sort.Strings(calls)
	want := []string{
		"hook opencode permission-ask cwd=/home/teen/my-app,model=claude-sonnet-5-5,session_id=ses_v2",
		"hook opencode permission-reply cwd=/home/teen/my-app,decision=deny,model=claude-sonnet-5-5,session_id=ses_v2",
		"hook opencode prompt cwd=/home/teen/my-app,model=claude-sonnet-5-5,prompt_chars=" + strconv.Itoa(aievent.Runes("héllo "+harnesstest.Sentinel)) + ",session_id=ses_v2",
		"hook opencode prompt cwd=/home/teen/my-app,prompt_chars=2,session_id=ses_resumed",
		"hook opencode turn-end cwd=/home/teen/my-app,session_id=ses_resumed",
		"hook opencode prompt cwd=/home/teen/v1-app,model=gpt-5.5,prompt_chars=" + strconv.Itoa(aievent.Runes("abc "+harnesstest.Sentinel)) + ",session_id=ses_v1",
		"hook opencode session-end cwd=/home/teen/my-app,model=claude-sonnet-5-5,session_id=ses_v2",
		"hook opencode session-start cwd=/home/teen/my-app,model=claude-sonnet-5-5,session_id=ses_v2",
		"hook opencode session-start cwd=/home/teen/v1-app,session_id=ses_v1",
		"hook opencode tool-end cwd=/home/teen/my-app,model=claude-sonnet-5-5,session_id=ses_v2,tool=bash",
		"hook opencode tool-start cwd=/home/teen/my-app,model=claude-sonnet-5-5,session_id=ses_v2,tool=bash",
		"hook opencode tool-start cwd=/home/teen/v1-app,model=gpt-5.5,session_id=ses_v1,tool=read",
		"hook opencode turn-end cwd=/home/teen/my-app,model=claude-sonnet-5-5,session_id=ses_v2",
		"hook opencode turn-end cwd=/home/teen/v1-app,model=gpt-5.5,session_id=ses_v1",
	}
	sort.Strings(want)
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plugin calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func toString(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.Itoa(int(f))
	}
	s, _ := v.(string)
	return s
}
