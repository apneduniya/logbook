package audit

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/submission"
)

// Write prints the report.
func (r Report) Write(w io.Writer) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	s := r.Submission
	names := map[string]string{}
	for _, d := range r.Devices {
		names[d.ID] = d.Name
	}
	p("Submission %s: %s, %s to %s (created %s)", s.ID, s.Project, s.From, s.To, r.created())
	for _, d := range r.Devices {
		p("Device %s (%s)", d.Name, d.ID)
		if d.Chain != nil {
			p("  signatures and chain  FAIL  %v", d.Chain)
		} else {
			p("  signatures and chain  OK    %s", count(d.Records, "signed record"))
		}
		switch {
		case !d.HasPart:
			p("  proofs                none  no part: its %s are unbacked", count(d.Rows, "frozen row"))
		case d.Chain != nil:
			p("  proofs                not checked")
		case d.Proofs != nil:
			p("  proofs and replays    FAIL  %v", d.Proofs)
		default:
			p("  proofs and replays    OK    %s, %s listed", count(d.Blocks, "block"), count(d.Listed, "minute"))
		}
	}
	if r.Cap != nil {
		p("Cap                     FAIL  %v", r.Cap)
	} else {
		p("Cap                     OK    %s recomputed from %s", count(r.CapMinutes, "capped minute"), count(r.CapInputs, "cap input"))
	}
	if r.Err == nil {
		p("Minutes                 %s verified, %s mismatch, %s unbacked",
			num(r.Minutes.Verified), num(r.Minutes.Mismatch), num(r.Minutes.Unbacked))
		for _, reason := range r.reasons() {
			p("  unbacked: %s (%s)", count(r.Unbacked[reason], "row"), reason)
		}
		for _, m := range r.Mismatches {
			p("  mismatch: %s %s: device %s, server %s", m.Row.Minute.UTC().Format(time.RFC3339), names[m.Row.DeviceID],
				credit(submission.Credit{Label: m.Row.Label, Credited: m.Row.Credited, Cuts: m.Row.Cuts}), credit(*m.Server))
		}
		if more := r.MismatchRows - len(r.Mismatches); more > 0 {
			p("  and %s more", count(more, "mismatched row"))
		}
		p("Counted                 %.1f h (%s s), from the frozen capped seconds", float64(r.CountedSeconds)/3600, num(int(r.CountedSeconds)))
	}
	switch {
	case r.OK():
		p("Result                  OK: every signature, chain, proof, replay and the cap check out")
	case r.Err != nil:
		p("Result                  FAIL: %v", r.Err)
	default:
		p("Result                  FAIL")
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

func credit(c submission.Credit) string {
	cuts := slices.Clone(c.Cuts)
	slices.Sort(cuts)
	s := fmt.Sprintf("%s %d s", c.Label, c.Credited)
	if len(cuts) > 0 {
		s += " cut " + strings.Join(cuts, ",")
	}
	return s
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return num(n) + " " + noun + "s"
}

// num formats n with thousands separators.
func num(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
