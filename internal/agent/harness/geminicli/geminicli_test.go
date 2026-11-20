package geminicli

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

// Gemini CLI settings may contain comments and trailing commas.
const userSettings = `{
  // personal settings
  "general": {"vimMode": true, "preferredEditor": "code"},
  "ui": {"theme": "GitHub"},
  "hooksConfig": {"enabled": true},
  "hooks": {
    "BeforeTool": [
      {
        "matcher": "write_file|replace",
        "hooks": [{"name": "secret-scanner", "type": "command", "command": "$GEMINI_PROJECT_DIR/.gemini/hooks/block-secrets.sh", "timeout": 5000}],
      },
    ],
  },
  /* MCP servers */
  "mcpServers": {"github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"]}},
}
`

func newInstaller(env hostenv.Env) harnesstest.Installer { return NewInstaller(env) }

func TestInstallRoundTrip(t *testing.T) {
	harnesstest.JSONInstallRoundTrip(t, newInstaller, SettingsPath, true, []harnesstest.InstallCase{
		{Name: "missing"},
		{Name: "existing_jsonc", Existing: userSettings},
	})
}

// On Windows Gemini CLI runs hooks with PowerShell, so a quoted path needs
// the call operator.
func TestWindowsCommand(t *testing.T) {
	bin := `C:\Users\Teen Dev\AppData\Local\Programs\Logbook\logbook.exe`
	es := entries(cmdline.PowerShell, bin)
	var h handler
	if err := json.Unmarshal(es[0].Handler, &h); err != nil {
		t.Fatal(err)
	}
	want := `& 'C:\Users\Teen Dev\AppData\Local\Programs\Logbook\logbook.exe' hook gemini-cli SessionStart`
	if h.Command != want {
		t.Fatalf("command = %s, want %s", h.Command, want)
	}
	if !cmdline.IsHook(hookfile.CommandArgv(es[0].Handler), Name) {
		t.Fatal("Windows entry not recognised as Logbook's")
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		event, fixture string
		want           wire.AIEvent
	}{
		{"SessionStart", "SessionStart.json", wire.AIEvent{Kind: wire.AISessionStart}},
		{"BeforeAgent", "BeforeAgent.json", wire.AIEvent{Kind: wire.AIPromptSubmit, PromptChars: aievent.Runes("Explain this repo, ünïcode " + harnesstest.Sentinel)}},
		{"BeforeTool", "BeforeTool.json", wire.AIEvent{Kind: wire.AIToolStart, Tool: "run_shell_command"}},
		{"AfterTool", "AfterTool.json", wire.AIEvent{Kind: wire.AIToolEnd, Tool: "run_shell_command"}},
		{"Notification", "NotificationPermission.json", wire.AIEvent{Kind: wire.AIPermissionAsk}},
		{"AfterAgent", "AfterAgent.json", wire.AIEvent{Kind: wire.AITurnEnd}},
		{"SessionEnd", "SessionEnd.json", wire.AIEvent{Kind: wire.AISessionEnd}},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			got := harnesstest.ParseFixture(t, wire.HarnessGeminiCLI, Parse, tc.event, tc.fixture)
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			want := tc.want
			want.Time, want.Harness, want.SessionID = harnesstest.Now(), wire.HarnessGeminiCLI, "9f1c-gem"
			want.Cwd, want.Project = "/home/teen/my-app", "my-app"
			if got[0] != want {
				t.Fatalf("got  %+v\nwant %+v", got[0], want)
			}
		})
	}
}

func TestParseSkipsOtherNotifications(t *testing.T) {
	in := aievent.Input{Data: harnesstest.Fixture(t, "NotificationOther.json")}
	if got := Parse("Notification", in, nil); got != nil {
		t.Fatalf("Parse = %+v, want nil", got)
	}
}

func TestResponseEmpty(t *testing.T) {
	for _, e := range Events() {
		if r := Response(e.Event); r != nil {
			t.Fatalf("Response(%s) = %q, want nothing", e.Event, r)
		}
	}
}
