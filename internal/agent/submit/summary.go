package submit

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// writeSummary prints what the upload holds and reveals (ADR 0020 step 4).
func writeSummary(w io.Writer, req Request, s Summary) {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	switch {
	case req.AddTo != "":
		p("Adding this device's part to submission %s (%s, %s to %s).", req.AddTo, req.Project, req.From, req.To)
	case req.Program != "":
		p("Submitting %s, %s to %s, to program %s.", req.Project, req.From, req.To, req.Program)
	default:
		p("Submitting %s, %s to %s.", req.Project, req.From, req.To)
	}

	p("\nFrom this device:")
	if s.Minutes == 0 {
		p("  no minute it can back with evidence")
	} else {
		p("  %s on %s (%d with credit), proven by %s%s", plural(s.Minutes, "minute"), plural(len(s.Days), "day"),
			s.Credited, plural(len(s.Blocks), "sealed block"), span(s.Blocks))
	}
	if len(s.Unbacked) > 0 {
		p("  Not backed (the review page shows them as not yet backed by evidence):")
		for _, c := range s.Unbacked {
			p("    %s: %s", plural(c.Minutes, "minute"), c.What)
		}
	}
	if len(s.Others) > 0 {
		p("From your other devices (no evidence until each adds its part with --add):")
		for _, c := range s.Others {
			p("  %s: %s", c.What, plural(c.Minutes, "minute"))
		}
	}

	if s.Minutes > 0 {
		r := s.Revealed
		p("\nWhat the server will see, besides each minute's label and credit:")
		p("  Every logged row in each minute's replay window (the two hours its run read), so it can re-run the rules:")
		p("    %s: %s", r.Project.Project, rows(r.Project))
		if len(r.Others) > 0 {
			p("    Other projects in those windows (their file paths and AI session details are included too):")
			for _, o := range r.Others {
				name := o.Project
				if name == "" {
					name = "(no project)"
				}
				p("      %s: %s", name, rows(o))
			}
		}
		var loose []string
		if r.Activity > 0 {
			loose = append(loose, plural(r.Activity, "activity sample")+" (idle seconds and the focused app's name)")
		}
		if r.ActiveTime > 0 {
			loose = append(loose, plural(r.ActiveTime, "Claude Code active-time row"))
		}
		if len(loose) > 0 {
			p("    Not tied to a project: %s", strings.Join(loose, ", "))
		}
		if len(r.Repos) > 0 {
			p("  Repo folders and their projects, from the opened repo buckets:")
			for _, repo := range r.Repos {
				p("    %s -> %s", repo.Path, repo.Project)
			}
		}
	}
	p("\nNever included: prompt text, code, keystrokes or file contents (the agent never records them). " +
		"AI events carry a prompt's length and a keyed hash of it, which can't be reversed.")
	_, _ = io.WriteString(w, b.String()) // a failed terminal write surfaces at the prompt or result
}

// rows describes a project's revealed rows.
func rows(p ProjectRows) string {
	var parts []string
	if p.Heartbeats > 0 {
		parts = append(parts, plural(p.Heartbeats, "editor heartbeat"))
	}
	if p.AIEvents > 0 {
		parts = append(parts, plural(p.AIEvents, "AI event"))
	}
	if p.Git > 0 {
		parts = append(parts, plural(p.Git, "git event"))
	}
	if len(parts) == 0 {
		return "0 rows"
	}
	return plural(p.Total(), "row") + " (" + strings.Join(parts, ", ") + ")"
}

// span describes a list of days by its ends.
func span(days []string) string {
	switch len(days) {
	case 0:
		return ""
	case 1:
		return " (" + days[0] + ")"
	}
	return " (" + days[0] + " to " + days[len(days)-1] + ")"
}

// plural is "1 minute" or "1,234 minutes".
func plural(n int, noun string) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if n == 1 {
		return s + " " + noun
	}
	return s + " " + noun + "s"
}
