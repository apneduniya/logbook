package copilot

import (
	"testing"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/harnesstest"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Logbook owns its own hook file, but a user could still have edited it.
const editedByUser = `{
  "version": 1,
  "hooks": {
    "preToolUse": [{"type": "command", "bash": "./scripts/check.sh", "powershell": "./scripts/check.ps1", "timeoutSec": 30}],
    "sessionStart": [{"type": "command", "bash": "/old/logbook hook copilot sessionStart"}]
  }
}
`

func newInstaller(env hostenv.Env) harnesstest.Installer { return NewInstaller(env) }

func TestInstallRoundTrip(t *testing.T) {
	harnesstest.JSONInstallRoundTrip(t, newInstaller, HooksPath, false, []harnesstest.InstallCase{
		{Name: "missing"},
		{Name: "existing", Existing: `{"version": 1, "hooks": {"preToolUse": [{"type": "command", "bash": "./scripts/check.sh", "timeoutSec": 30}]}}`},
	})
}

// A stale Logbook entry in shell form is replaced by the exec-form one; the
// user's own entry stays.
func TestInstallReplacesShellFormEntry(t *testing.T) {
	env, _ := harnesstest.Env(t)
	path, err := HooksPath(env)
	if err != nil {
		t.Fatal(err)
	}
	harnesstest.WriteFile(t, path, editedByUser)
	in := NewInstaller(env)
	if err := in.Install(harnesstest.Bin); err != nil {
		t.Fatal(err)
	}
	harnesstest.Golden(t, "install_edited_by_user", harnesstest.ReadFile(t, path))
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	harnesstest.SameJSON(t, harnesstest.ReadFile(t, path), []byte(`{"version": 1, "hooks": {"preToolUse": [{"type": "command", "bash": "./scripts/check.sh", "powershell": "./scripts/check.ps1", "timeoutSec": 30}]}}`), false)
}

func TestNoPreToolUse(t *testing.T) {
	if _, ok := Events().Kind("preToolUse"); ok {
		t.Fatal("preToolUse fails closed in Copilot; a missing binary would block every tool call")
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		event, fixture string
		want           wire.AIEvent
	}{
		{"sessionStart", "sessionStart.json", wire.AIEvent{Kind: wire.AISessionStart}},
		{"userPromptSubmitted", "userPromptSubmitted.json", wire.AIEvent{Kind: wire.AIPromptSubmit, PromptChars: aievent.Runes("Add a dark mode toggle 🌙 " + harnesstest.Sentinel)}},
		{"postToolUse", "postToolUse.json", wire.AIEvent{Kind: wire.AIToolEnd, Tool: "bash"}},
		{"postToolUseFailure", "postToolUseFailure.json", wire.AIEvent{Kind: wire.AIToolEnd, Tool: "edit"}},
		{"notification", "notificationPermission.json", wire.AIEvent{Kind: wire.AIPermissionAsk}},
		{"agentStop", "agentStop.json", wire.AIEvent{Kind: wire.AITurnEnd}},
		{"sessionEnd", "sessionEnd.json", wire.AIEvent{Kind: wire.AISessionEnd}},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			got := harnesstest.ParseFixture(t, wire.HarnessCopilot, Parse, tc.event, tc.fixture)
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			want := tc.want
			want.Time, want.Harness, want.SessionID = harnesstest.Now(), wire.HarnessCopilot, "sess-cp-1"
			want.Cwd, want.Project = "/home/teen/my-app", "my-app"
			if got[0] != want {
				t.Fatalf("got  %+v\nwant %+v", got[0], want)
			}
		})
	}
}

func TestParseSkipsOtherNotifications(t *testing.T) {
	in := aievent.Input{Data: harnesstest.Fixture(t, "notificationShell.json")}
	if got := Parse("notification", in, nil); got != nil {
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
