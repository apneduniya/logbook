package localkey

import (
	"encoding/hex"
	"errors"
	"testing"
)

const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The derivations are part of the on-disk contract (settings.json holds an
// OTLP token, stored events hold prompt hashes), so their outputs are pinned.
// The expected values were cross-checked with an independent RFC 5869
// implementation (Python hmac).
func TestDerivationsArePinned(t *testing.T) {
	pk, err := PromptKey(token)
	if err != nil {
		t.Fatal(err)
	}
	ot, err := OTLPToken(token)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, got, want string }{
		{"prompt key", hex.EncodeToString(pk), "ebe5060e151a933527ca0950d1debe3952758ae4492d9a2141b76413aeff127c"},
		{"otlp token", ot, "59cd87cdb5c577d2173d433b577a1d803cfe94ca7b299b96492b7ff1c3a477c1"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestDerivationsDiffer(t *testing.T) {
	pk, _ := PromptKey(token)
	ot, _ := OTLPToken(token)
	other, _ := OTLPToken(token[:len(token)-1] + "0")
	switch {
	case hex.EncodeToString(pk) == ot:
		t.Fatal("prompt key and OTLP token are the same")
	case ot == other:
		t.Fatal("different local tokens gave the same OTLP token")
	case ot == token:
		t.Fatal("OTLP token is the local token")
	}
}

func TestShortTokenRefused(t *testing.T) {
	for _, tok := range []string{"", "short", token[:31]} {
		if _, err := PromptKey(tok); !errors.Is(err, ErrNoToken) {
			t.Errorf("PromptKey(%q) err = %v", tok, err)
		}
		if _, err := OTLPToken(tok); !errors.Is(err, ErrNoToken) {
			t.Errorf("OTLPToken(%q) err = %v", tok, err)
		}
	}
}
