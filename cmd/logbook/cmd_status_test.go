package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/crediting"
)

func TestPrintCreditingShowsClockProblem(t *testing.T) {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	printCrediting(p, crediting.Status{Minutes: crediting.MinutesStatus{Clock: "the system clock is behind the last sealed day"}})
	if !strings.Contains(b.String(), "clock: the system clock is behind the last sealed day") {
		t.Fatalf("output:\n%s", b.String())
	}
}
