package evidence

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func testDay() Day {
	from, to := time.Date(2026, 11, 20, 8, 59, 0, 0, time.UTC), time.Date(2026, 11, 20, 17, 30, 0, 0, time.UTC)
	return Day{
		DeviceID:     "0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b",
		Day:          "2026-11-20",
		ContentHash:  strings.Repeat("ab", 32),
		RulesVersion: "v1",
		SeqFrom:      40,
		SeqTo:        5321,
		Buckets:      412,
		TimeFrom:     &from,
		TimeTo:       &to,
	}
}

// repoOnlyDay is a block with only its repo bucket: no time span.
func repoOnlyDay() Day {
	d := testDay()
	d.PrevChainHash = strings.Repeat("cd", 32)
	d.Buckets, d.TimeFrom, d.TimeTo = 1, nil, nil
	return d
}

// The exact bytes the chain hash covers, and the hashes, are pinned so
// agent and server can never silently disagree on the format.
func TestChainMessageGolden(t *testing.T) {
	var buf bytes.Buffer
	for _, d := range []Day{testDay(), repoOnlyDay()} {
		buf.WriteString(hex.EncodeToString(chainMessage(d)))
		buf.WriteByte('\n')
	}
	golden(t, "chain_message.golden", buf.Bytes())
	for _, tt := range []struct {
		d    Day
		want string
	}{
		{testDay(), "fc9866e84aef48738a64f10a4a1859342fa499786b8c3d20c74173432f9c7a04"},
		{repoOnlyDay(), "1de9fd9406df3c01b152bedc8ff5a07cc94b2fd09bb02977321c503e945a2fa7"},
	} {
		got, err := ChainHash(tt.d)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("ChainHash = %s, want %s (format changed? bump chainDomain)", got, tt.want)
		}
	}
}

// Every field is in the chain hash, and no two different Days share a
// message: a null time differs from the Unix epoch, and moving bytes from
// one string to the next changes the length prefixes.
func TestChainMessageCoversEveryField(t *testing.T) {
	base := chainMessage(testDay())
	epoch := time.Unix(0, 0).UTC()
	for name, mutate := range map[string]func(*Day){
		"device":     func(d *Day) { d.DeviceID = "1b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b" },
		"day":        func(d *Day) { d.Day = "2026-11-21" },
		"prev":       func(d *Day) { d.PrevChainHash = strings.Repeat("cd", 32) },
		"content":    func(d *Day) { d.ContentHash = strings.Repeat("ef", 32) },
		"rules":      func(d *Day) { d.RulesVersion = "v2" },
		"seq_from":   func(d *Day) { d.SeqFrom++ },
		"seq_to":     func(d *Day) { d.SeqTo++ },
		"buckets":    func(d *Day) { d.Buckets++ },
		"time_from":  func(d *Day) { f := d.TimeFrom.Add(-time.Minute); d.TimeFrom = &f },
		"time_to":    func(d *Day) { f := d.TimeTo.Add(time.Minute); d.TimeTo = &f },
		"null times": func(d *Day) { d.TimeFrom, d.TimeTo = nil, nil },
		"epoch":      func(d *Day) { d.TimeFrom = &epoch },
		"shifted":    func(d *Day) { d.RulesVersion, d.Day = d.Day[:4], d.Day[4:]+d.RulesVersion },
	} {
		d := testDay()
		mutate(&d)
		if bytes.Equal(chainMessage(d), base) {
			t.Errorf("%s: message unchanged", name)
		}
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
	for name, mutate := range map[string]func(*Day){
		"content":  func(d *Day) { d.ContentHash = strings.Repeat("cd", 32) },
		"seq_to":   func(d *Day) { d.SeqTo++ },
		"buckets":  func(d *Day) { d.Buckets-- },
		"time_to":  func(d *Day) { to := d.TimeTo.Add(time.Minute); d.TimeTo = &to },
		"seq_from": func(d *Day) { d.SeqFrom-- },
	} {
		tampered := testDay()
		mutate(&tampered)
		if err := Verify(pub, tampered, ch, sig); !errors.Is(err, ErrBadSignature) {
			t.Errorf("Verify(%s tampered) = %v, want ErrBadSignature", name, err)
		}
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if err := Verify(otherPub, d, ch, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify(other key) = %v, want ErrBadSignature", err)
	}
}

func TestValidate(t *testing.T) {
	at := func(h, m, s int) *time.Time { v := time.Date(2026, 11, 20, h, m, s, 0, time.UTC); return &v }
	tests := []struct {
		name   string
		mutate func(*Day)
	}{
		{"bad day", func(d *Day) { d.Day = "20-11-2026" }},
		{"bad device", func(d *Day) { d.DeviceID = "x" }},
		{"uppercase hash", func(d *Day) { d.ContentHash = strings.Repeat("AB", 32) }},
		{"short prev", func(d *Day) { d.PrevChainHash = "abc" }},
		{"no rules", func(d *Day) { d.RulesVersion = "" }},
		{"negative seq_from", func(d *Day) { d.SeqFrom = -1 }},
		{"empty seq range", func(d *Day) { d.SeqTo = d.SeqFrom }},
		{"backwards seq range", func(d *Day) { d.SeqTo = d.SeqFrom - 1 }},
		{"no buckets", func(d *Day) { d.Buckets = 0 }},
		{"only time_from", func(d *Day) { d.TimeTo = nil }},
		{"only time_to", func(d *Day) { d.TimeFrom = nil }},
		{"times reversed", func(d *Day) { d.TimeFrom, d.TimeTo = d.TimeTo, d.TimeFrom }},
		{"not a whole minute", func(d *Day) { d.TimeTo = at(17, 30, 1) }},
		{"before 1970", func(d *Day) { e := time.Unix(-60, 0); d.TimeFrom = &e }},
		{"null times, more buckets", func(d *Day) { d.TimeFrom, d.TimeTo = nil, nil }},
		{"times, one bucket", func(d *Day) { d.Buckets = 1 }},
		{"more buckets than minutes", func(d *Day) { d.TimeFrom, d.TimeTo = at(9, 0, 0), at(9, 1, 0); d.Buckets = 4 }},
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
	ok := testDay()
	ok.SeqFrom, ok.TimeFrom, ok.TimeTo, ok.Buckets = 0, at(9, 0, 0), at(9, 1, 0), 3
	if err := ok.Validate(); err != nil {
		t.Fatalf("a first block over two minutes: %v", err)
	}
	if err := repoOnlyDay().Validate(); err != nil {
		t.Fatalf("a repo-only block: %v", err)
	}
}
