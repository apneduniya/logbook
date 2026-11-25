package submission

import (
	"fmt"
	"strconv"
	"strings"
)

// Justification is the paragraph a program pastes into Airtable's
// "Justification - Alternate Tracking Method" field (ADR 0020): the
// packet's counts, its rules version and the review link. It is built
// when the packet is served, never stored, because it holds the link.
func Justification(p Packet, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Tracked with Logbook (rules %s). %s counted of %s at the keyboard; the one-third AI rule would give %s.",
		p.RulesVersion, hours(p.CountedSeconds), hours(p.RawSeconds), hours(p.OneThirdRuleSeconds))
	total := p.Minutes.Verified + p.Minutes.Mismatch + p.Minutes.Unbacked
	if p.Minutes.Mismatch == 0 && p.Minutes.Unbacked == 0 {
		fmt.Fprintf(&b, " All %s minutes re-computed on our server from signed device logs.", thousands(total))
	} else {
		fmt.Fprintf(&b, " %s of %s minutes re-computed on our server from signed device logs; %s differ from the device's result and %s are not yet backed by evidence.",
			thousands(p.Minutes.Verified), thousands(total), thousands(p.Minutes.Mismatch), thousands(p.Minutes.Unbacked))
	}
	fmt.Fprintf(&b, " Review: %s", link)
	return b.String()
}

// hours formats seconds as hours with one decimal: "12.4 h".
func hours(secs int64) string {
	return strconv.FormatFloat(float64(secs)/3600, 'f', 1, 64) + " h"
}

// thousands formats n with commas: 2431 is "2,431".
func thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
