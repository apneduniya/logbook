package evidence

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
)

func testDay() Day {
	return Day{
		DeviceID:     "0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b",
		Day:          "2026-11-20",
		ContentHash:  strings.Repeat("ab", 32),
		RulesVersion: "v1",
	}
}

func TestChainHashGolden(t *testing.T) {
	// Pinned so agent and server can never silently disagree on the format.
	got, err := ChainHash(testDay())
	if err != nil {
		t.Fatal(err)
	}
	const want = "4592bad6f7249eaaf98f5ff49d054cc560b0e9ca764f4829c2d2ed992f96bcef"
	if got != want {
		t.Fatalf("ChainHash = %s, want %s (format changed? bump chainDomain)", got, want)
	}
}

func TestSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := testDay()
	ch, sig, err := Sign(priv, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, d, ch, sig); err != nil {
		t.Fatalf("Verify() = %v", err)
	}

	tampered := d
	tampered.ContentHash = strings.Repeat("cd", 32)
	if err := Verify(pub, tampered, ch, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify(tampered) = %v, want ErrBadSignature", err)
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if err := Verify(otherPub, d, ch, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify(other key) = %v, want ErrBadSignature", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Day)
	}{
		{"bad day", func(d *Day) { d.Day = "20-11-2026" }},
		{"bad device", func(d *Day) { d.DeviceID = "x" }},
		{"uppercase hash", func(d *Day) { d.ContentHash = strings.Repeat("AB", 32) }},
		{"short prev", func(d *Day) { d.PrevChainHash = "abc" }},
		{"no rules", func(d *Day) { d.RulesVersion = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDay()
			tt.mutate(&d)
			if _, err := ChainHash(d); !errors.Is(err, ErrInvalidDay) {
				t.Fatalf("ChainHash() err = %v, want ErrInvalidDay", err)
			}
		})
	}
}
