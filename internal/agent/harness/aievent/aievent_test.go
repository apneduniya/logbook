package aievent

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

var now = time.Date(2026, 11, 20, 10, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))

func TestBuild(t *testing.T) {
	base := func(string) string { return "proj" }
	tests := []struct {
		name string
		f    Fields
		want wire.AIEvent
	}{
		{"basic", Fields{Kind: wire.AIToolStart, SessionID: "s", Cwd: "/w/p", Model: "m", Tool: "Bash"},
			wire.AIEvent{Kind: wire.AIToolStart, SessionID: "s", Cwd: "/w/p", Project: "proj", Model: "m", Tool: "Bash"}},
		{"prompt chars only on prompt_submit", Fields{Kind: wire.AITurnEnd, PromptChars: 9},
			wire.AIEvent{Kind: wire.AITurnEnd}},
		{"prompt chars", Fields{Kind: wire.AIPromptSubmit, PromptChars: 9},
			wire.AIEvent{Kind: wire.AIPromptSubmit, PromptChars: 9}},
		{"bad decision dropped", Fields{Kind: wire.AIPermissionAsk, Decision: "maybe"},
			wire.AIEvent{Kind: wire.AIPermissionAsk}},
		{"decision kept", Fields{Kind: wire.AIPermissionAsk, Decision: "deny"},
			wire.AIEvent{Kind: wire.AIPermissionAsk, Decision: "deny"}},
		{"control chars stripped", Fields{Kind: wire.AIToolEnd, Tool: "Ba\x00sh\n", SessionID: " s\t1 "},
			wire.AIEvent{Kind: wire.AIToolEnd, Tool: "Bash", SessionID: "s1"}},
		{"long cwd dropped", Fields{Kind: wire.AIToolEnd, Cwd: "/" + strings.Repeat("a", 2000)},
			wire.AIEvent{Kind: wire.AIToolEnd}},
		{"cwd with newline dropped", Fields{Kind: wire.AIToolEnd, Cwd: "/a\nb"},
			wire.AIEvent{Kind: wire.AIToolEnd}},
		{"ago", Fields{Kind: wire.AIToolStart, Ago: 2 * time.Second},
			wire.AIEvent{Kind: wire.AIToolStart, Time: now.Add(-2 * time.Second).UTC()}},
		{"ago too big ignored", Fields{Kind: wire.AIToolStart, Ago: 48 * time.Hour},
			wire.AIEvent{Kind: wire.AIToolStart}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Build(wire.HarnessCodex, tc.f, now, base)
			want := tc.want
			want.Harness = wire.HarnessCodex
			if want.Time.IsZero() {
				want.Time = now.UTC()
			}
			if got != want {
				t.Fatalf("got  %+v\nwant %+v", got, want)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestCleanCutsOnRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 200) // 400 bytes
	got := Clean(s, 255)
	if len(got) != 254 || !strings.HasPrefix(s, got) {
		t.Fatalf("Clean cut to %d bytes", len(got))
	}
	if Clean("ok\xff", 10) != "ok" {
		t.Fatal("invalid UTF-8 not removed")
	}
}

func TestProjectName(t *testing.T) {
	repo := filepath.FromSlash("/home/teen/code/my-app")
	exists := func(p string) bool { return p == filepath.Join(repo, ".git") }
	tests := map[string]string{
		repo:                                   "my-app",
		filepath.Join(repo, "src", "lib"):      "my-app",
		filepath.FromSlash("/home/teen/notes"): "notes",
		filepath.FromSlash("/"):                "",
	}
	for cwd, want := range tests {
		if got := ProjectName(cwd, exists); got != want {
			t.Errorf("ProjectName(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestReadInputCaps(t *testing.T) {
	big := bytes.Repeat([]byte("x"), MaxPayload+10)
	in := ReadInput(context.Background(), bytes.NewReader(big))
	if !in.Truncated || len(in.Data) != MaxPayload {
		t.Fatalf("Truncated=%v len=%d", in.Truncated, len(in.Data))
	}
	in = ReadInput(context.Background(), strings.NewReader(`{"a":1}`))
	if in.Truncated || string(in.Data) != `{"a":1}` {
		t.Fatalf("small input: %+v", in)
	}
}

// A harness that never closes stdin must not hang the hook.
func TestReadInputTimeout(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	in := ReadInput(ctx, r)
	if time.Since(start) > 2*time.Second || !in.Truncated || len(in.Data) != 0 {
		t.Fatalf("ReadInput didn't give up: %+v after %v", in, time.Since(start))
	}
}

func TestSalvage(t *testing.T) {
	prefix := `{"session_id":"s1","n":3,"ok":true,"nested":{"a":[1,{"b":2}]},"cwd":"/w","tool_response":"cut her`
	got := string(Salvage([]byte(prefix)))
	want := `{"cwd":"/w","n":3,"ok":true,"session_id":"s1"}`
	if got != want {
		t.Fatalf("Salvage = %s, want %s", got, want)
	}
	if string(Salvage([]byte(`[1,2`))) != "{}" || string(Salvage(nil)) != "{}" {
		t.Fatal("non-object prefix should salvage to {}")
	}
}

func TestDecodeTolerant(t *testing.T) {
	var p struct {
		SessionID string  `json:"session_id"`
		Prompt    TextLen `json:"prompt"`
		Tool      string  `json:"tool_name"`
	}
	if err := Decode(Input{Data: []byte(`{"session_id": 5, "prompt": "héllo", "tool_name": "Edit"}`)}, &p); err != nil {
		t.Fatal(err)
	}
	if p.SessionID != "" || p.Prompt != 5 || p.Tool != "Edit" {
		t.Fatalf("decoded %+v", p)
	}
	if err := Decode(Input{Data: []byte("not json")}, &p); err == nil {
		t.Fatal("garbage decoded without error")
	}
	if err := Decode(Input{}, &p); err != nil {
		t.Fatalf("empty input: %v", err)
	}
}

func TestTable(t *testing.T) {
	tab := Table{{Event: "A", Kind: wire.AISessionStart}, {Event: "B", Kind: wire.AITurnEnd}}
	if k, ok := tab.Kind("B"); !ok || k != wire.AITurnEnd {
		t.Fatal("Kind(B)")
	}
	if _, ok := tab.Kind("C"); ok {
		t.Fatal("Kind(C) found")
	}
	if got := tab.Names(); len(got) != 2 || got[0] != "A" {
		t.Fatalf("Names = %v", got)
	}
}
