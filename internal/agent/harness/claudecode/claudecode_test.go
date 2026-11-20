package claudecode

import (
	"testing"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/harnesstest"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/pkg/wire"
)

const userSettings = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "model": "opus",
  "permissions": {"allow": ["Bash(npm test:*)"], "deny": []},
  "statusLine": {"type": "command", "command": "~/.claude/statusline.sh"},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "~/.claude/hooks/check-bash.sh", "timeout": 30}]}
    ],
    "Notification": [
      {"matcher": "idle_prompt", "hooks": [{"type": "command", "command": "notify-send 'Claude is idle'"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "http", "url": "http://localhost:9000/stop"}]}
    ]
  },
  "enabledPlugins": {"formatter@acme": true},
  "futureSetting": {"nested": [1, 2.50, "x"], "flag": null}
}
`

// A user who put their own handler in the same group as an older Logbook.
const sharedGroup = `{
  "hooks": {
    "SessionStart": [
      {"hooks": [
        {"type": "command", "command": "/old/place/logbook", "args": ["hook", "claude-code", "SessionStart"]},
        {"type": "command", "command": "echo started"}
      ]}
    ],
    "SubagentStop": [
      {"hooks": [{"type": "command", "command": "/old/place/logbook hook claude-code SubagentStop"}]}
    ]
  }
}
`

func newInstaller(env hostenv.Env) harnesstest.Installer { return NewInstaller(env) }

func TestInstallRoundTrip(t *testing.T) {
	harnesstest.JSONInstallRoundTrip(t, newInstaller, SettingsPath, false, []harnesstest.InstallCase{
		{Name: "missing"},
		{Name: "empty_file", Existing: "\n"},
		{Name: "existing", Existing: userSettings},
	})
}

// An older install (other path, and an event Logbook no longer uses) is
// replaced, and the user's handler sharing a group with it survives.
func TestInstallReplacesOlderEntries(t *testing.T) {
	env, _ := harnesstest.Env(t)
	path, err := SettingsPath(env)
	if err != nil {
		t.Fatal(err)
	}
	harnesstest.WriteFile(t, path, sharedGroup)
	in := NewInstaller(env)
	if err := in.Install(harnesstest.Bin); err != nil {
		t.Fatal(err)
	}
	harnesstest.Golden(t, "install_shared_group", harnesstest.ReadFile(t, path))
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	harnesstest.SameJSON(t, harnesstest.ReadFile(t, path), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo started"}]}]}}`), false)
}

func TestInstallRejectsBadFiles(t *testing.T) {
	for name, content := range map[string]string{
		"not_json":     `{"hooks": `,
		"hooks_array":  `{"hooks": []}`,
		"event_object": `{"hooks": {"Stop": {"command": "x"}}}`,
		"top_array":    `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			env, _ := harnesstest.Env(t)
			path, _ := SettingsPath(env)
			harnesstest.WriteFile(t, path, content)
			if err := NewInstaller(env).Install(harnesstest.Bin); err == nil {
				t.Fatal("Install succeeded on a malformed file")
			}
			if got := string(harnesstest.ReadFile(t, path)); got != content {
				t.Fatalf("malformed file was changed to %q", got)
			}
		})
	}
}

func TestInstallRelativeBin(t *testing.T) {
	env, _ := harnesstest.Env(t)
	if err := NewInstaller(env).Install("logbook"); err == nil {
		t.Fatal("Install accepted a relative binary path")
	}
}

func TestSettingsPathHonoursConfigDir(t *testing.T) {
	env, _ := harnesstest.Env(t)
	env.Getenv = func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return "/custom/claude"
		}
		return ""
	}
	got, err := SettingsPath(env)
	if err != nil || got != "/custom/claude/settings.json" {
		t.Fatalf("SettingsPath = %q, %v", got, err)
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		event, fixture string
		want           wire.AIEvent
	}{
		{"SessionStart", "SessionStart.json", wire.AIEvent{Kind: wire.AISessionStart, Model: "claude-opus-5"}},
		{"UserPromptSubmit", "UserPromptSubmit.json", wire.AIEvent{Kind: wire.AIPromptSubmit, PromptChars: aievent.Runes("Write a function ✨ " + harnesstest.Sentinel)}},
		{"PreToolUse", "PreToolUse.json", wire.AIEvent{Kind: wire.AIToolStart, Tool: "Bash"}},
		{"PostToolUse", "PostToolUse.json", wire.AIEvent{Kind: wire.AIToolEnd, Tool: "Bash"}},
		{"PostToolUseFailure", "PostToolUseFailure.json", wire.AIEvent{Kind: wire.AIToolEnd, Tool: "Bash"}},
		{"PermissionRequest", "PermissionRequest.json", wire.AIEvent{Kind: wire.AIPermissionAsk, Tool: "Bash"}},
		{"Stop", "Stop.json", wire.AIEvent{Kind: wire.AITurnEnd}},
		{"StopFailure", "StopFailure.json", wire.AIEvent{Kind: wire.AITurnEnd}},
		{"SessionEnd", "SessionEnd.json", wire.AIEvent{Kind: wire.AISessionEnd}},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			got := harnesstest.ParseFixture(t, wire.HarnessClaudeCode, Parse, tc.event, tc.fixture)
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			want := tc.want
			want.Time, want.Harness, want.SessionID = harnesstest.Now(), wire.HarnessClaudeCode, "abc123"
			want.Cwd, want.Project = "/home/teen/my-app", "my-app"
			if got[0] != want {
				t.Fatalf("got  %+v\nwant %+v", got[0], want)
			}
		})
	}
}

func TestParseSkips(t *testing.T) {
	cases := map[string]struct {
		event, fixture string
		env            string
	}{
		"not installed event": {"Notification", "Notification.json", ""},
		"run by Cursor":       {"PreToolUse", "CursorPreToolUse.json", ""},
		"Cursor env":          {"PreToolUse", "PreToolUse.json", "2.3.1"},
		"unknown event":       {"Bogus", "PreToolUse.json", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := aievent.Input{Data: harnesstest.Fixture(t, tc.fixture)}
			getenv := func(k string) string {
				if k == "CURSOR_VERSION" {
					return tc.env
				}
				return ""
			}
			if got := Parse(tc.event, in, getenv); got != nil {
				t.Fatalf("Parse = %+v, want nil", got)
			}
		})
	}
}

// A real Claude Code session (CLAUDE_CODE_CHILD_SESSION=1 on its hooks)
// started from a shell that inherited Cursor's environment is still
// recorded; only Cursor-run hooks are dropped.
func TestCursorDedup(t *testing.T) {
	cases := []struct {
		name, fixture string
		env           map[string]string
		kept          bool
	}{
		{"plain Claude Code", "PreToolUse.json", nil, true},
		{"Claude Code with Cursor env leaked", "PreToolUse.json", map[string]string{"CURSOR_VERSION": "2.3.1", "CLAUDE_CODE_CHILD_SESSION": "1"}, true},
		{"run by Cursor, env only", "PreToolUse.json", map[string]string{"CURSOR_VERSION": "2.3.1"}, false},
		{"run by Cursor, payload field", "CursorPreToolUse.json", nil, false},
		{"payload field wins over child-session env", "CursorPreToolUse.json", map[string]string{"CLAUDE_CODE_CHILD_SESSION": "1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := aievent.Input{Data: harnesstest.Fixture(t, tc.fixture)}
			got := Parse("PreToolUse", in, func(k string) string { return tc.env[k] })
			if (got != nil) != tc.kept {
				t.Fatalf("Parse = %+v, want kept=%v", got, tc.kept)
			}
		})
	}
}

func TestParseTruncated(t *testing.T) {
	data := harnesstest.BigPayload(`"session_id":"abc123","cwd":"/home/teen/my-app","hook_event_name":"PostToolUse","tool_name":"Bash"`)
	in := aievent.Input{Data: data[:aievent.MaxPayload], Truncated: true}
	got := harnesstest.Build(t, wire.HarnessClaudeCode, Parse("PostToolUse", in, func(string) string { return "" }))
	if len(got) != 1 || got[0].SessionID != "abc123" || got[0].Tool != "Bash" || got[0].Project != "my-app" {
		t.Fatalf("truncated payload parsed to %+v", got)
	}
}

func TestParseGarbage(t *testing.T) {
	for _, data := range []string{"", "not json", "[]", `{"session_id": 7, "prompt": {"x": 1}}`} {
		got := Parse("UserPromptSubmit", aievent.Input{Data: []byte(data)}, func(string) string { return "" })
		for _, f := range got {
			if f.PromptChars != 0 || f.SessionID != "" {
				t.Fatalf("garbage %q parsed to %+v", data, f)
			}
		}
	}
}

func TestResponseEmpty(t *testing.T) {
	for _, e := range Events() {
		if r := Response(e.Event); r != nil {
			t.Fatalf("Response(%s) = %q, want nothing", e.Event, r)
		}
	}
}
