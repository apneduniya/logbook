package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/pkg/evidence"
)

func TestPrintCreditingShowsClockProblem(t *testing.T) {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	printCrediting(p, crediting.Status{Minutes: crediting.MinutesStatus{Clock: "the system clock is behind the last sealed day"}})
	if !strings.Contains(b.String(), "clock: the system clock is behind the last sealed day") {
		t.Fatalf("output:\n%s", b.String())
	}
}

// status prints the device key's fingerprint, which an auditor compares
// with the keys in a verification bundle (`logbook verify --key`).
func TestStatusShowsKeyFingerprint(t *testing.T) {
	dir := t.TempDir()
	if got := keyFingerprint(dir); got != "" {
		t.Fatalf("fingerprint without a key = %q", got)
	}
	k, err := devicekey.Create(config.DeviceKeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	want := evidence.KeyFingerprint(k.Public())
	if got := keyFingerprint(dir); got != want || len(got) != 16 {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
	var b strings.Builder
	printStatus(&b, status.Snapshot{}, "127.0.0.1:1", want)
	if !strings.Contains(b.String(), "Key:        "+want) {
		t.Fatalf("output:\n%s", b.String())
	}
}
