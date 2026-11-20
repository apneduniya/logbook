package hookrun_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/harness"
	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/harnesstest"
	"github.com/apneduniya/logbook/internal/agent/harness/hookrun"
	"github.com/apneduniya/logbook/internal/agent/harness/localpost"
	"github.com/apneduniya/logbook/pkg/wire"
)

// payloads is one realistic payload per harness, each with private text.
func payloads() map[string]struct {
	event, body, stdout string
	kind                wire.AIEventKind
} {
	s := harnesstest.Sentinel
	return map[string]struct {
		event, body, stdout string
		kind                wire.AIEventKind
	}{
		"claude-code": {"UserPromptSubmit", `{"session_id":"a","cwd":"/w/app","hook_event_name":"UserPromptSubmit","prompt":"hi ` + s + `"}`, "", wire.AIPromptSubmit},
		"codex":       {"Stop", `{"session_id":"b","cwd":"/w/app","model":"gpt","last_assistant_message":"` + s + `"}`, "", wire.AITurnEnd},
		"gemini-cli":  {"AfterTool", `{"session_id":"c","cwd":"/w/app","tool_name":"read_file","tool_response":{"llmContent":"` + s + `"}}`, "", wire.AIToolEnd},
		"opencode":    {"tool-start", `{"session_id":"d","cwd":"/w/app","tool":"bash"}`, "", wire.AIToolStart},
		"cursor":      {"beforeSubmitPrompt", `{"conversation_id":"e","workspace_roots":["/w/app"],"prompt":"` + s + `"}`, `{"continue":true}` + "\n", wire.AIPromptSubmit},
		"copilot":     {"sessionEnd", `{"sessionId":"f","cwd":"/w/app","reason":"user_exit"}`, "", wire.AISessionEnd},
	}
}

func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func runner(stdin io.Reader, stdout io.Writer, sender func() (hookrun.Sender, error), logged *[]string) *hookrun.Runner {
	return &hookrun.Runner{
		Stdin:   stdin,
		Stdout:  stdout,
		Now:     harnesstest.Now,
		Getenv:  func(string) string { return "" },
		Project: func(cwd string) string { return filepath.Base(cwd) },
		Sender:  sender,
		Log:     func(msg string, _ ...any) { *logged = append(*logged, msg) },
	}
}

// With the daemon down, every harness's hook spools its event, prints exactly
// the harness's protocol response, and leaks nothing private.
func TestDaemonDownSpools(t *testing.T) {
	for name, tc := range payloads() {
		t.Run(name, func(t *testing.T) {
			spool := filepath.Join(t.TempDir(), "spool", "events.jsonl")
			hk, ok := harness.Lookup(name)
			if !ok {
				t.Fatalf("harness %s not registered", name)
			}
			var out bytes.Buffer
			var logged []string
			sender := func() (hookrun.Sender, error) {
				return localpost.New(closedAddr(t), strings.Repeat("t", 64), spool, slog.New(slog.NewTextHandler(io.Discard, nil))), nil
			}
			start := time.Now()
			runner(strings.NewReader(tc.body), &out, sender, &logged).Run(context.Background(), hk.Harness, hk, tc.event)
			if d := time.Since(start); d > time.Second {
				t.Fatalf("hook took %v with the daemon down", d)
			}
			if out.String() != tc.stdout {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.stdout)
			}
			if len(logged) != 0 {
				t.Fatalf("logged %v", logged)
			}
			raw, err := os.ReadFile(spool) //nolint:gosec // test path
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), harnesstest.Sentinel) {
				t.Fatalf("private text spooled: %s", raw)
			}
			sc := bufio.NewScanner(bytes.NewReader(raw))
			var n int
			for sc.Scan() {
				var e wire.LocalEvent
				if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Validate() != nil {
					t.Fatalf("bad spool line %s", sc.Text())
				}
				if e.AI.Harness != wire.Harness(name) || e.AI.Kind != tc.kind || e.AI.Project != "app" {
					t.Fatalf("spooled %+v", e.AI)
				}
				n++
			}
			if n != 1 {
				t.Fatalf("spooled %d events, want 1", n)
			}
		})
	}
}

type recordSender struct{ got []wire.LocalEvent }

func (r *recordSender) Send(_ context.Context, ev []wire.LocalEvent) error {
	r.got = append(r.got, ev...)
	return nil
}

// Stdin bigger than the cap still yields the event's metadata, and the
// payload's private text is never kept.
func TestOversizedStdin(t *testing.T) {
	body := harnesstest.BigPayload(`"session_id":"big","cwd":"/w/app","tool_name":"Bash"`)
	hk, _ := harness.Lookup("claude-code")
	rec := &recordSender{}
	var logged []string
	var out bytes.Buffer
	r := runner(bytes.NewReader(body), &out, func() (hookrun.Sender, error) { return rec, nil }, &logged)
	r.Run(context.Background(), hk.Harness, hk, "PostToolUse")
	if len(rec.got) != 1 || rec.got[0].AI.SessionID != "big" || rec.got[0].AI.Tool != "Bash" {
		t.Fatalf("got %d events, first %+v", len(rec.got), rec.got[0].AI)
	}
	harnesstest.NoLeak(t, rec.got)
}

func TestUnknownHarnessOrEvent(t *testing.T) {
	var logged []string
	called := false
	sender := func() (hookrun.Sender, error) { called = true; return &recordSender{}, nil }
	var out bytes.Buffer
	runner(strings.NewReader(`{}`), &out, sender, &logged).Run(context.Background(), "", nil, "x")
	hk, _ := harness.Lookup("codex")
	runner(strings.NewReader(`{}`), &out, sender, &logged).Run(context.Background(), hk.Harness, hk, "NotAnEvent")
	if called || out.Len() != 0 || len(logged) != 0 {
		t.Fatalf("called=%v out=%q logged=%v", called, out.String(), logged)
	}
}

func TestSenderErrorIsLoggedNotFatal(t *testing.T) {
	hk, _ := harness.Lookup("cursor")
	var logged []string
	var out bytes.Buffer
	r := runner(strings.NewReader(`{"conversation_id":"e"}`), &out, func() (hookrun.Sender, error) {
		return nil, errors.New("no data dir")
	}, &logged)
	r.Run(context.Background(), hk.Harness, hk, "stop")
	if out.String() != "{}\n" || len(logged) != 1 {
		t.Fatalf("out=%q logged=%v", out.String(), logged)
	}
}

type panicParser struct{}

func (panicParser) Parse(string, aievent.Input, func(string) string) []aievent.Fields { panic("boom") }
func (panicParser) Response(string) []byte                                            { return []byte("{}") }

func TestPanicIsContained(t *testing.T) {
	var logged []string
	var out bytes.Buffer
	runner(strings.NewReader(`{}`), &out, nil, &logged).Run(context.Background(), wire.HarnessCursor, panicParser{}, "stop")
	if out.String() != "{}" || len(logged) != 1 {
		t.Fatalf("out=%q logged=%v", out.String(), logged)
	}
}
