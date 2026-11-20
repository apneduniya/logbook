package aievent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/pkg/wire"
)

func TestNormalizePrompt(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"   \t\n", ""},
		{"Hello World", "hello world"},
		{"  hello   world  ", "hello world"},
		{"hello\r\n\tworld", "hello world"},
		{"hello world　", "hello world"},  // NBSP and ideographic space
		{"İstanbul ΣΑΣ", "istanbul σασ"}, // rune-wise lowercasing
		{"ok\xffgo", "okgo"},             // invalid UTF-8 dropped
		{"Fix   the\nBUG in main.go", "fix the bug in main.go"},
	}
	for _, tc := range tests {
		if got := NormalizePrompt(tc.in); got != tc.want {
			t.Errorf("NormalizePrompt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPromptDecode(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	tests := []struct {
		name     string
		json     string
		runes    int
		hashed   bool
		sameAsHi bool
	}{
		{"string", `"héllo"`, 5, true, false},
		{"same as hi after normalising", `"  HI "`, 5, true, true},
		{"empty string", `""`, 0, true, false},
		{"salvaged length", `1234`, 1234, false, false},
		{"negative", `-3`, 0, false, false},
		{"object", `{"text":"hi"}`, 0, false, false},
		{"null", `null`, 0, false, false},
	}
	hi := DigestPrompt("hi").Keyed(key)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p Prompt
			if err := json.Unmarshal([]byte(tc.json), &p); err != nil {
				t.Fatal(err)
			}
			if p.Len() != tc.runes || p.Digest().IsSet() != tc.hashed {
				t.Fatalf("Len=%d hashed=%v", p.Len(), p.Digest().IsSet())
			}
			if got := p.Digest().Keyed(key); (got == hi) != tc.sameAsHi {
				t.Fatalf("keyed = %q, hi = %q", got, hi)
			}
		})
	}
}

// A Prompt never carries its text: not in its JSON form, not in %v/%+v.
func TestPromptKeepsNoText(t *testing.T) {
	const secret = "SENTINEL-PRIVATE"
	var p struct {
		Prompt Prompt `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(`{"prompt":"say `+secret+`"}`), &p); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	for _, s := range []string{string(b), strings.ToLower(string(b)), sprint(p), sprint(&p)} {
		if strings.Contains(strings.ToLower(s), strings.ToLower(secret)) {
			t.Fatalf("prompt text leaked: %s", s)
		}
	}
	if string(b) != `{"prompt":20}` {
		t.Fatalf("marshalled %s", b)
	}
}

func TestPromptDigestKeyed(t *testing.T) {
	k1, k2 := []byte("key-one-key-one-key-one-key-one!"), []byte("key-two-key-two-key-two-key-two!")
	d := DigestPrompt("hello")
	switch {
	case d.Keyed(k1) == d.Keyed(k2):
		t.Fatal("different keys, same hash")
	case d.Keyed(k1) != DigestPrompt("  HELLO\n").Keyed(k1):
		t.Fatal("normalised variants differ")
	case d.Keyed(nil) != "" || (PromptDigest{}).Keyed(k1) != "":
		t.Fatal("missing key or digest should give no hash")
	case len(d.Keyed(k1)) != 64:
		t.Fatal("hash is not 64 hex characters")
	}
	if got := ParsePromptDigest(d.Hex()); got != d {
		t.Fatal("Hex/ParsePromptDigest round trip failed")
	}
	for _, bad := range []string{"", "zz", strings.Repeat("g", 64), d.Hex()[:62]} {
		if ParsePromptDigest(bad).IsSet() {
			t.Errorf("ParsePromptDigest(%q) is set", bad)
		}
	}
}

func TestPromptHashOnlyForPromptSubmit(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	f := Fields{Kind: wire.AIToolStart, PromptDigest: DigestPrompt("x")}
	if PromptHash(f, key) != "" {
		t.Fatal("hash on a tool event")
	}
	f.Kind = wire.AIPromptSubmit
	if PromptHash(f, key) == "" {
		t.Fatal("no hash on a prompt")
	}
}

func sprint(v any) string { return fmt.Sprintf("%v %+v %#v", v, v, v) }
