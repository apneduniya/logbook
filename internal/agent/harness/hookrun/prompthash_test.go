package hookrun_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/harness"
	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/internal/agent/harness/harnesstest"
	"github.com/apneduniya/logbook/internal/agent/harness/hookrun"
	"github.com/apneduniya/logbook/internal/agent/localkey"
	"github.com/apneduniya/logbook/pkg/wire"
)

const (
	tokenA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// promptPayload builds a prompt_submit payload for a harness with prompt
// text p.
func promptPayload(t *testing.T, name, p string) (event, body string) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	q := string(b)
	switch name {
	case "claude-code":
		return "UserPromptSubmit", `{"session_id":"s","cwd":"/w/app","prompt":` + q + `}`
	case "codex":
		return "UserPromptSubmit", `{"session_id":"s","cwd":"/w/app","model":"gpt","prompt":` + q + `}`
	case "gemini-cli":
		return "BeforeAgent", `{"session_id":"s","cwd":"/w/app","prompt":` + q + `}`
	case "cursor":
		return "beforeSubmitPrompt", `{"conversation_id":"s","workspace_roots":["/w/app"],"prompt":` + q + `}`
	case "copilot":
		return "userPromptSubmitted", `{"sessionId":"s","cwd":"/w/app","prompt":` + q + `}`
	case "opencode":
		// The plugin digests the prompt itself (TestPluginInNode checks it
		// matches DigestPrompt) and sends only the digest.
		return "prompt", `{"session_id":"s","cwd":"/w/app","prompt_chars":3,"prompt_sha256":"` + aievent.DigestPrompt(p).Hex() + `"}`
	}
	t.Fatalf("no prompt payload for %s", name)
	return "", ""
}

func promptEvent(t *testing.T, name, token, prompt string) wire.AIEvent {
	t.Helper()
	hk, ok := harness.Lookup(name)
	if !ok {
		t.Fatalf("harness %s not registered", name)
	}
	event, body := promptPayload(t, name, prompt)
	rec := &recordSender{}
	var logged []string
	var out bytes.Buffer
	r := runner(strings.NewReader(body), &out, func() (hookrun.Sender, error) { return rec, nil }, &logged)
	r.PromptKey = func() ([]byte, error) { return localkey.PromptKey(token) }
	r.Run(context.Background(), hk.Harness, hk, event)
	if len(rec.got) != 1 || len(logged) != 0 {
		t.Fatalf("%s: got %d events, logged %v", name, len(rec.got), logged)
	}
	harnesstest.NoLeak(t, rec.got)
	return *rec.got[0].AI
}

// Every harness that sees prompt text records the same keyed hash for the
// same prompt, the same hash for spacing and case variants, a different one
// for a different prompt or another device's token, and never the text.
func TestPromptHashAllHarnesses(t *testing.T) {
	s := harnesstest.Sentinel
	base := "Fix the " + s + " test"
	for _, name := range []string{"claude-code", "codex", "gemini-cli", "cursor", "copilot", "opencode"} {
		t.Run(name, func(t *testing.T) {
			ev := promptEvent(t, name, tokenA, base)
			if ev.Kind != wire.AIPromptSubmit || len(ev.PromptHash) != 64 {
				t.Fatalf("event %+v", ev)
			}
			if _, err := hex.DecodeString(ev.PromptHash); err != nil {
				t.Fatalf("hash not hex: %q", ev.PromptHash)
			}
			if err := ev.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, variant := range []string{
				"  fix THE " + strings.ToLower(s) + "   test\t",
				"\nFix the " + s + "\r\ntest　",
			} {
				if got := promptEvent(t, name, tokenA, variant).PromptHash; got != ev.PromptHash {
					t.Errorf("variant %q hashed differently", variant)
				}
			}
			if promptEvent(t, name, tokenA, "Fix the other test").PromptHash == ev.PromptHash {
				t.Error("different prompt, same hash")
			}
			if promptEvent(t, name, tokenB, base).PromptHash == ev.PromptHash {
				t.Error("different token, same hash")
			}
		})
	}
}

// The hash is the documented construction:
// hex(HMAC-SHA256(PromptKey(token), SHA-256(NormalizePrompt(prompt)))).
// The expected value was computed independently (Python hmac/hashlib).
func TestPromptHashPinned(t *testing.T) {
	ev := promptEvent(t, "claude-code", tokenA, "  Hello \n  World ")
	const want = "c496d67af0c2c9a5660e898528d134631f32a679c5738c03d8f5afcedbc66b79"
	if ev.PromptHash != want {
		t.Fatalf("PromptHash = %s, want %s", ev.PromptHash, want)
	}
}

// Without a key (no config yet) or with a key error, the event is still
// recorded, just without a hash.
func TestPromptHashWithoutKey(t *testing.T) {
	hk, _ := harness.Lookup("claude-code")
	for i, key := range []func() ([]byte, error){
		nil,
		func() ([]byte, error) { return nil, errors.New("no config") },
		func() ([]byte, error) { return nil, nil },
	} {
		rec := &recordSender{}
		var logged []string
		var out bytes.Buffer
		r := runner(strings.NewReader(`{"session_id":"s","prompt":"hello"}`), &out,
			func() (hookrun.Sender, error) { return rec, nil }, &logged)
		r.PromptKey = key
		r.Run(context.Background(), hk.Harness, hk, "UserPromptSubmit")
		if len(rec.got) != 1 || rec.got[0].AI.PromptHash != "" || rec.got[0].AI.PromptChars != 5 {
			t.Fatalf("case %d: got %+v", i, rec.got)
		}
	}
}

// Other events never ask for the key (it costs a config read).
func TestPromptKeyOnlyForPrompts(t *testing.T) {
	hk, _ := harness.Lookup("claude-code")
	rec := &recordSender{}
	var logged []string
	var out bytes.Buffer
	r := runner(strings.NewReader(`{"session_id":"s","prompt":"x","tool_name":"Bash"}`), &out,
		func() (hookrun.Sender, error) { return rec, nil }, &logged)
	r.PromptKey = func() ([]byte, error) { t.Error("key read for a tool event"); return nil, nil }
	r.Run(context.Background(), hk.Harness, hk, "PreToolUse")
	if len(rec.got) != 1 || rec.got[0].AI.PromptHash != "" {
		t.Fatalf("got %+v", rec.got)
	}
}

// A prompt cut off by the payload cap keeps its length but can't be hashed.
func TestTruncatedPromptHasNoHash(t *testing.T) {
	hk, _ := harness.Lookup("claude-code")
	body := `{"session_id":"s","prompt":"` + strings.Repeat("a", aievent.MaxPayload)
	rec := &recordSender{}
	var logged []string
	var out bytes.Buffer
	r := runner(strings.NewReader(body), &out, func() (hookrun.Sender, error) { return rec, nil }, &logged)
	r.PromptKey = func() ([]byte, error) { t.Error("key read for a prompt with no digest"); return nil, nil }
	r.Run(context.Background(), hk.Harness, hk, "UserPromptSubmit")
	if len(rec.got) != 1 || rec.got[0].AI.PromptHash != "" || rec.got[0].AI.PromptChars == 0 {
		t.Fatalf("got %+v", rec.got)
	}
}
