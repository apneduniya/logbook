package cursor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/cmdline"
	"github.com/apneduniya/logbook/internal/agent/harness/harnesstest"
	"github.com/apneduniya/logbook/internal/agent/harness/hookfile"
	"github.com/apneduniya/logbook/internal/agent/harness/hostenv"
	"github.com/apneduniya/logbook/pkg/wire"
)

const userHooks = `{
  "version": 1,
  "hooks": {
    "afterFileEdit": [{ "command": "./hooks/format.sh" }],
    "stop": [{ "command": "./audit.sh", "loop_limit": 10 }],
    "beforeShellExecution": [{ "command": "./approve-network.sh", "timeout": 30, "matcher": "curl|wget|nc" }]
  }
}
`

func newInstaller(env hostenv.Env) harnesstest.Installer { return NewInstaller(env) }

func TestInstallRoundTrip(t *testing.T) {
	harnesstest.JSONInstallRoundTrip(t, newInstaller, HooksPath, false, []harnesstest.InstallCase{
		{Name: "missing"},
		{Name: "existing", Existing: userHooks},
		{Name: "no_version", Existing: `{"hooks": {}, "custom": "kept"}`},
	})
}

// On Windows the desktop app runs hook commands through PowerShell.
func TestWindowsCommand(t *testing.T) {
	bin := `C:\Users\Teen Dev\AppData\Local\Programs\Logbook\logbook.exe`
	es := entries(cmdline.PowerShell, bin)
	var h handler
	if err := json.Unmarshal(es[0].Handler, &h); err != nil {
		t.Fatal(err)
	}
	want := `& 'C:\Users\Teen Dev\AppData\Local\Programs\Logbook\logbook.exe' hook cursor sessionStart`
	if h.Command != want {
		t.Fatalf("command = %s, want %s", h.Command, want)
	}
	if !cmdline.IsHook(hookfile.CommandArgv(es[0].Handler), Name) {
		t.Fatal("Windows entry not recognised as Logbook's")
	}
	plain := entries(cmdline.PowerShell, `C:\Logbook\logbook.exe`)
	if err := json.Unmarshal(plain[0].Handler, &h); err != nil {
		t.Fatal(err)
	}
	if h.Command != `C:\Logbook\logbook.exe hook cursor sessionStart` {
		t.Fatalf("unquoted command = %s", h.Command)
	}
}

func TestNoPermissionHooks(t *testing.T) {
	for _, e := range Events() {
		switch e.Event {
		case "preToolUse", "beforeShellExecution", "beforeMCPExecution", "beforeReadFile", "beforeTabFileRead", "subagentStart":
			t.Fatalf("%s is a permission hook; installing it could block or auto-approve actions", e.Event)
		}
	}
}

func TestParse(t *testing.T) {
	base := wire.AIEvent{Time: harnesstest.Now(), Harness: wire.HarnessCursor, SessionID: "conv-42",
		Model: "claude-opus-4-7", Cwd: "/home/teen/my-app", Project: "my-app"}
	with := func(f func(*wire.AIEvent)) wire.AIEvent { e := base; f(&e); return e }
	tests := []struct {
		event string
		want  []wire.AIEvent
	}{
		{"sessionStart", []wire.AIEvent{with(func(e *wire.AIEvent) { e.Kind = wire.AISessionStart })}},
		{"beforeSubmitPrompt", []wire.AIEvent{with(func(e *wire.AIEvent) {
			e.Kind, e.PromptChars = wire.AIPromptSubmit, aievent.Runes("Refactor the header ✏️ "+harnesstest.Sentinel)
		})}},
		{"postToolUse", []wire.AIEvent{
			with(func(e *wire.AIEvent) {
				e.Kind, e.Tool, e.Time = wire.AIToolStart, "Shell", harnesstest.Now().Add(-5432*time.Millisecond)
			}),
			with(func(e *wire.AIEvent) { e.Kind, e.Tool = wire.AIToolEnd, "Shell" }),
		}},
		{"postToolUseFailure", []wire.AIEvent{
			with(func(e *wire.AIEvent) {
				e.Kind, e.Tool, e.Time = wire.AIToolStart, "Read", harnesstest.Now().Add(-12*time.Millisecond)
			}),
			with(func(e *wire.AIEvent) { e.Kind, e.Tool = wire.AIToolEnd, "Read" }),
		}},
		{"stop", []wire.AIEvent{with(func(e *wire.AIEvent) { e.Kind = wire.AITurnEnd })}},
		{"sessionEnd", []wire.AIEvent{with(func(e *wire.AIEvent) { e.Kind = wire.AISessionEnd })}},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			got := harnesstest.ParseFixture(t, wire.HarnessCursor, Parse, tc.event, tc.event+".json")
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("event %d\ngot  %+v\nwant %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseSkipsUninstalledEvents(t *testing.T) {
	in := aievent.Input{Data: harnesstest.Fixture(t, "afterAgentResponse.json")}
	if got := Parse("afterAgentResponse", in, nil); got != nil {
		t.Fatalf("Parse = %+v, want nil", got)
	}
}

func TestResponse(t *testing.T) {
	for _, e := range Events() {
		r := Response(e.Event)
		var v map[string]any
		if err := json.Unmarshal(r, &v); err != nil {
			t.Fatalf("Response(%s) = %q is not a JSON object", e.Event, r)
		}
		if e.Event == "beforeSubmitPrompt" && v["continue"] != true {
			t.Fatalf("beforeSubmitPrompt response %q doesn't continue", r)
		}
		if _, ok := v["permission"]; ok {
			t.Fatalf("Response(%s) makes a permission decision", e.Event)
		}
	}
}
