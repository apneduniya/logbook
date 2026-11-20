package struggle

import (
	"slices"
	"time"
)

// Compute labels and credits every minute in in.Window for one device,
// following docs/STRUGGLE-RULES.md at RulesVersion. It is deterministic:
// the same Input always yields the same Result, whatever the order of its
// slices. Events outside the window are context only. Minutes come sorted by
// (minute, project); idle minutes with no project are left out.
func Compute(in Input) Result {
	p := prepare(in)
	res := Result{RulesVersion: RulesVersion, Minutes: []Minute{}}
	if !p.to.After(p.from) {
		return res
	}
	turns := buildTurns(p.ai, p.horizon)
	markAutomated(turns, p.samples, p.instants)
	tl := newTimeline(p, turns)

	rows := make([][]Minute, tl.n) // per minute, sorted by project
	for i := tl.first; i < tl.last; i++ {
		rows[i] = tl.label(i, p.events)
	}

	minutes := func(spans []span) []bool {
		set := make([]bool, tl.n)
		for _, s := range spans {
			tl.mark(set, s.from, s.to)
		}
		return set
	}
	// Turn cuts take the time spent on the cut turn. A minute with a human
	// heartbeat was spent editing, whatever the agent did, so it keeps its
	// credit.
	cut := func(spans []span, code string) {
		set := minutes(spans)
		for i := tl.first; i < tl.last; i++ {
			if set[i] && !tl.humanHB[i] {
				cutYou(rows[i], code)
			}
		}
	}
	cut(loopSpans(turns, p.heartbeats), CutLoop)
	cut(rubberStampSpans(turns, p.heartbeats), CutRubberStamp)

	flagSpans := func(spans []span, code string) {
		set := minutes(spans)
		for i := tl.first; i < tl.last; i++ {
			if set[i] {
				flagAll(rows[i], code)
			}
		}
	}
	flagSpans(automatedSpans(turns), FlagAutomatedSession)
	flagSpans(scriptedSpans(p.heartbeats, p.samples), FlagScriptedInput)

	for i := tl.first; i < tl.last; i++ {
		if tl.lowSignal[i] {
			flagYou(rows[i], FlagLowSignal)
		}
	}
	flagOTelMismatch(tl, rows, turns, p)
	flagLongDay(tl, rows, p.tz)

	for i := tl.first; i < tl.last; i++ {
		for _, m := range rows[i] {
			slices.Sort(m.Cuts)
			slices.Sort(m.Flags)
			res.Minutes = append(res.Minutes, m)
		}
	}
	return res
}

// label decides minute i's label and returns its rows, sorted by project.
func (tl *timeline) label(i int, events []projectEvent) []Minute {
	start := tl.minute(i)
	end := tl.minute(i + 1)
	recent := func() string { return lastProject(events, start.Add(-projectLookback), end) }
	row := func(project string, label Label, credited int, cuts ...string) Minute {
		return Minute{Minute: start, Project: project, Label: label, Credited: credited,
			Cuts: append([]string{}, cuts...), Flags: []string{}}
	}

	if tl.recentlyPresent(i) && tl.focusAllowed(i) {
		// Split the minute by the person's own signals; only without any,
		// by every event (an agent's or editor's) that names a project.
		counts := tl.humanProjects[i]
		if len(counts) == 0 {
			counts = tl.allProjects[i]
		}
		if len(counts) > 0 {
			var out []Minute
			for _, s := range splitSeconds(60, counts) {
				out = append(out, row(s.project, LabelYou, s.count))
			}
			return out
		}
		// No event in the minute: the latest project of the last 5
		// minutes, else of the last 15 (e.g. reading docs), else of the
		// person's next signal within 5 minutes (e.g. composing the first
		// prompt of a session).
		project := recent()
		if project == "" {
			project = lastProject(events, start.Add(-activeSessionWindow), end)
		}
		if project == "" {
			project = firstProject(tl.humanEvents, end, end.Add(projectLookback))
		}
		// Input with no project and no active session is not evidence of
		// any work: it falls through to idle.
		if project != "" || tl.activeSession(i) {
			return []Minute{row(project, LabelYou, 60)}
		}
	}
	if tl.busy[i] {
		project := tl.busyProj[i]
		if project == "" {
			project = recent()
		}
		return []Minute{row(project, LabelAgent, 0, CutAgentAlone)}
	}
	project := recent()
	if project == "" {
		return nil
	}
	if tl.backgroundEditor(i) {
		return []Minute{row(project, LabelIdle, 0, CutBackgroundEditor)}
	}
	return []Minute{row(project, LabelIdle, 0, CutIdle)}
}

// splitSeconds divides total seconds between projects in proportion to their
// event counts, by largest remainder (ties: project name). Projects that get
// no whole second are dropped. counts must be sorted by project.
func splitSeconds(total int, counts []projectCount) []projectCount {
	sum := 0
	for _, c := range counts {
		sum += c.count
	}
	out := make([]projectCount, len(counts))
	order := make([]int, len(counts))
	left := total
	for i, c := range counts {
		out[i] = projectCount{c.project, total * c.count / sum}
		left -= out[i].count
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		// Larger remainder first; the stable sort keeps name order on ties.
		return (total * counts[b].count % sum) - (total * counts[a].count % sum)
	})
	for _, i := range order[:left] {
		out[i].count++
	}
	return slices.DeleteFunc(out, func(c projectCount) bool { return c.count == 0 })
}

// cutYou removes the credit of a minute's "you" rows under rule code.
func cutYou(rows []Minute, code string) {
	for k := range rows {
		if rows[k].Label == LabelYou {
			rows[k].Credited = 0
			rows[k].Cuts = appendOnce(rows[k].Cuts, code)
		}
	}
}

func flagAll(rows []Minute, code string) {
	for k := range rows {
		rows[k].Flags = appendOnce(rows[k].Flags, code)
	}
}

func flagYou(rows []Minute, code string) {
	for k := range rows {
		if rows[k].Label == LabelYou {
			rows[k].Flags = appendOnce(rows[k].Flags, code)
		}
	}
}

func appendOnce(list []string, code string) []string {
	if slices.Contains(list, code) {
		return list
	}
	return append(list, code)
}

// flagOTelMismatch compares, per Claude Code session with active-time data,
// the seconds credited while the person was in the harness (minutes where
// the session had an event or a turn, focus on a terminal, AI app or
// unknown) with the session's own active time over the window: user plus
// cli. Claude Code counts "user" for keyboard interactions and "cli" for
// tool execution and AI responses, so a person watching the agent work is
// covered by cli time; user time alone would flag them. When the active time
// is under otelMaxRatio of the credited time and short by more than
// otelMinGap, those minutes are flagged: the hooks claim far more harness
// time than Claude Code measured.
func flagOTelMismatch(tl *timeline, rows [][]Minute, turns []turn, p prepared) {
	activeSecs := map[string]float64{}
	var sessions []string
	for _, at := range p.activeTime { // sorted, so float sums are stable
		h := string(at.Harness)
		if h == "" {
			h = "claude-code"
		}
		key := h + "/" + at.SessionID
		if _, ok := activeSecs[key]; !ok {
			sessions = append(sessions, key)
		}
		secs := max(at.UserSecs, 0) + max(at.CLISecs, 0)
		activeSecs[key] += secs * overlapFraction(at.From, at.To, p.from, p.to)
	}
	for _, s := range sessions {
		in := make([]bool, tl.n)
		for _, i := range tl.sessionOf[s] {
			in[i] = true
		}
		for _, t := range turns {
			if t.session == s {
				tl.mark(in, t.start, t.end)
			}
		}
		credited := 0
		for i := tl.first; i < tl.last; i++ {
			switch tl.dominantFocus(i) {
			case focusTerminal, focusAI, focusUnknown:
			default:
				in[i] = false // the person was elsewhere, e.g. in the editor
			}
			if in[i] {
				for _, m := range rows[i] {
					credited += m.Credited
				}
			}
		}
		c, u := float64(credited), activeSecs[s]
		if u >= otelMaxRatio*c || c-u <= otelMinGap.Seconds() {
			continue
		}
		for i := tl.first; i < tl.last; i++ {
			if in[i] {
				flagYou(rows[i], FlagOTelMismatch)
			}
		}
	}
}

// overlapFraction is the share of [a, b) that lies inside [from, to); a
// zero-length datapoint counts fully when it is inside.
func overlapFraction(a, b, from, to time.Time) float64 {
	if !b.After(a) {
		if !a.Before(from) && a.Before(to) {
			return 1
		}
		return 0
	}
	lo, hi := maxTime(a, from), b
	if to.Before(hi) {
		hi = to
	}
	if !hi.After(lo) {
		return 0
	}
	return float64(hi.Sub(lo)) / float64(b.Sub(a))
}

// flagLongDay flags credited minutes once the running credited total of
// their local day (in tz), counted within the window, passes longDay. The
// server re-checks over whole days.
func flagLongDay(tl *timeline, rows [][]Minute, tz *time.Location) {
	var day string
	total := 0
	for i := tl.first; i < tl.last; i++ {
		if d := tl.minute(i).In(tz).Format(time.DateOnly); d != day {
			day, total = d, 0
		}
		credited := 0
		for _, m := range rows[i] {
			credited += m.Credited
		}
		total += credited
		if time.Duration(total)*time.Second <= longDay {
			continue
		}
		for k := range rows[i] {
			if rows[i][k].Credited > 0 {
				rows[i][k].Flags = appendOnce(rows[i][k].Flags, FlagLongDay)
			}
		}
	}
}
