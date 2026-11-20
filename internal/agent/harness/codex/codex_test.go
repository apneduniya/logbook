package codex

import (
	"encoding/json"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/cmdline"
	"github.com/apneduniya/logbook/internal/agent/harness/harnesstest"
	"github.com/apneduniya/logbook/internal/agent/harness/hookfile"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/pkg/wire"
)

const userHooks = `{
  "description": "my hooks",
  "hooks": {
    "SessionStart": [
      {"matcher": "startup|resume", "hooks": [{"type": "command", "command": "python3 ~/.codex/hooks/session_start.py", "statusMessage": "Loading notes"}]}
    ],
    "PreToolUse": [
      {"matcher": "^Bash$", "hooks": [{"type": "command", "command": "/usr/bin/python3 policy.py", "timeout": 30}]},
      {"matcher": "Write|Edit", "hooks": [{"type": "mcp_tool", "server": "scanner", "tool": "scan_patch"}]}
    ]
  },
  "x-unknown": {"keep": true}
}
`

func newInstaller(env hostenv.Env) harnesstest.Installer { return NewInstaller(env) }

func TestInstallRoundTrip(t *testing.T) {
	harnesstest.JSONInstallRoundTrip(t, newInstaller, HooksPath, false, []harnesstest.InstallCase{
		{Name: "missing"},
		{Name: "existing", Existing: userHooks},
	})
}

func TestHooksPathHonoursCodexHome(t *testing.T) {
	env, _ := harnesstest.Env(t)
	env.Getenv = func(k string) string {
		if k == "CODEX_HOME" {
			return "/srv/codex"
		}
		return ""
	}
	if got, err := HooksPath(env); err != nil || got != "/srv/codex/hooks.json" {
		t.Fatalf("HooksPath = %q, %v", got, err)
	}
}

// On Windows Codex runs hooks with cmd.exe, so a path with spaces is
// double-quoted, and the entry is still recognised as Logbook's.
func TestWindowsCommand(t *testing.T) {
	bin := `C:\Users\Teen Dev\AppData\Local\Programs\Logbook\logbook.exe`
	es := entries(cmdline.Cmd, bin)
	var h handler
	if err := json.Unmarshal(es[0].Handler, &h); err != nil {
		t.Fatal(err)
	}
	want := `"C:\Users\Teen Dev\AppData\Local\Programs\Logbook\logbook.exe" hook codex SessionStart`
	if h.Command != want {
		t.Fatalf("command = %s, want %s", h.Command, want)
	}
	if !cmdline.IsHook(hookfile.CommandArgv(es[0].Handler), Name) {
		t.Fatal("Windows entry not recognised as Logbook's")
	}
}

func TestShortTimeouts(t *testing.T) {
	for _, e := range entries(cmdline.POSIX, harnesstest.Bin) {
		var h handler
		if err := json.Unmarshal(e.Handler, &h); err != nil {
			t.Fatal(err)
		}
		limit := hookTimeout
		if e.Event == "SessionEnd" || e.Event == "Interrupt" {
			limit = 3
		}
		if h.Timeout != limit {
			t.Errorf("%s timeout = %d, want %d", e.Event, h.Timeout, limit)
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		event string
		want  wire.AIEvent
	}{
		{"SessionStart", wire.AIEvent{Kind: wire.AISessionStart}},
		{"UserPromptSubmit", wire.AIEvent{Kind: wire.AIPromptSubmit, PromptChars: aievent.Runes("Fix the failing test 🙏 " + harnesstest.Sentinel)}},
		{"PreToolUse", wire.AIEvent{Kind: wire.AIToolStart, Tool: "Bash"}},
		{"PostToolUse", wire.AIEvent{Kind: wire.AIToolEnd, Tool: "Bash"}},
		{"PermissionRequest", wire.AIEvent{Kind: wire.AIPermissionAsk, Tool: "apply_patch"}},
		{"Stop", wire.AIEvent{Kind: wire.AITurnEnd}},
		{"Interrupt", wire.AIEvent{Kind: wire.AITurnEnd}},
		{"SessionEnd", wire.AIEvent{Kind: wire.AISessionEnd}},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			got := harnesstest.ParseFixture(t, wire.HarnessCodex, Parse, tc.event, tc.event+".json")
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			want := tc.want
			want.Time, want.Harness, want.SessionID, want.Model = harnesstest.Now(), wire.HarnessCodex, "thr_123", "gpt-5.5-codex"
			want.Cwd, want.Project = "/home/teen/my-app", "my-app"
			if got[0] != want {
				t.Fatalf("got  %+v\nwant %+v", got[0], want)
			}
		})
	}
}

func TestParseSkipsUninstalledEvents(t *testing.T) {
	in := aievent.Input{Data: harnesstest.Fixture(t, "PreModelRequest.json")}
	if got := Parse("PreModelRequest", in, nil); got != nil {
		t.Fatalf("Parse = %+v, want nil", got)
	}
}

func TestParseTruncated(t *testing.T) {
	data := harnesstest.BigPayload(`"session_id":"thr_123","cwd":"/home/teen/my-app","model":"gpt-5.5-codex","tool_name":"Bash"`)
	in := aievent.Input{Data: data[:aievent.MaxPayload], Truncated: true}
	got := harnesstest.Build(t, wire.HarnessCodex, Parse("PostToolUse", in, nil))
	if len(got) != 1 || got[0].SessionID != "thr_123" || got[0].Tool != "Bash" || got[0].Model != "gpt-5.5-codex" {
		t.Fatalf("truncated payload parsed to %+v", got)
	}
}

func TestHasInlineHooks(t *testing.T) {
	tests := map[string]bool{
		"": false,
		"model = \"gpt-5.5\"\n[profiles.fast]\nx=1": false,
		"[hooks]\n": true,
		"  [[hooks.PreToolUse]]\nmatcher = \"^Bash$\"\n[[hooks.PreToolUse.hooks]]\ncommand = \"x\"": true,
		"[hooks.Stop] # mine":       true,
		"hooks.Stop = []\n":         true,
		"hooks = { }\n":             true,
		"[profiles.x]\nhooks = 1\n": false, // a key inside another table
		"[hookshot]\n":              false,
		"hooks_dir = \"x\"\n":       false,
	}
	for in, want := range tests {
		if got := HasInlineHooks([]byte(in)); got != want {
			t.Errorf("HasInlineHooks(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNotesWarnAboutInlineHooks(t *testing.T) {
	env, _ := harnesstest.Env(t)
	in := NewInstaller(env)
	if n := in.Notes(); len(n) != 0 {
		t.Fatalf("Notes with no config.toml = %v", n)
	}
	path, err := ConfigPath(env)
	if err != nil {
		t.Fatal(err)
	}
	harnesstest.WriteFile(t, path, "model = \"gpt-5.5\"\n")
	if n := in.Notes(); len(n) != 0 {
		t.Fatalf("Notes without inline hooks = %v", n)
	}
	harnesstest.WriteFile(t, path, "[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ncommand = \"./x.sh\"\n")
	if n := in.Notes(); len(n) != 1 {
		t.Fatalf("Notes with inline hooks = %v, want one note", n)
	}
	// config.toml is only read, never written.
	if got := string(harnesstest.ReadFile(t, path)); got != "[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ncommand = \"./x.sh\"\n" {
		t.Fatalf("config.toml changed: %q", got)
	}
}

func TestResponseEmpty(t *testing.T) {
	for _, e := range Events() {
		if r := Response(e.Event); r != nil {
			t.Fatalf("Response(%s) = %q, want nothing", e.Event, r)
		}
	}
}
