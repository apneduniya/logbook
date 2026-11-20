package struggle

import (
	"slices"
	"time"
)

// focusKind is wire.ActivitySample.FocusKind as an array index.
type focusKind int

const (
	focusEditor focusKind = iota
	focusTerminal
	focusAI
	focusBrowser
	focusOther
	focusUnknown
	numFocusKinds
)

func parseFocusKind(s string) focusKind {
	switch s {
	case "editor":
		return focusEditor
	case "terminal":
		return focusTerminal
	case "ai":
		return focusAI
	case "browser":
		return focusBrowser
	case "other":
		return focusOther
	}
	return focusUnknown
}

// timeline holds per-minute facts over [base, base+n minutes). It covers the
// window plus marginBefore minutes of context and marginAfter of lookahead;
// only the window's minutes, [first, last), are emitted.
type timeline struct {
	base        time.Time
	n           int
	first, last int

	samples   []int                    // activity samples in the minute
	kinds     [][numFocusKinds]int     // samples per focus kind
	lastKind  []focusKind              // focus of the minute's latest sample
	present   []bool                   // the person gave input in the minute
	lowSignal []bool                   // no working idle source near the minute
	hbs       []int                    // heartbeats in the minute
	moved     []bool                   // a human heartbeat with movement
	events    []int                    // heartbeats + AI events in the minute
	busy      []bool                   // an agent was working in the minute
	busyProj  []string                 // project of the latest busy turn
	projects  [][]projectCount         // named-project event counts, by project
	focusOK   []bool                   // computed by label
	engaged   []bool                   // computed by label
	sessionOf map[string][]minuteIndex // AI session -> minutes it had events in
}

type projectCount struct {
	project string
	count   int
}

type minuteIndex = int

func newTimeline(p prepared) *timeline {
	base := p.from.Add(-marginBefore * time.Minute)
	windowMinutes := int(p.to.Sub(p.from) / time.Minute)
	tl := &timeline{
		base:  base,
		n:     marginBefore + windowMinutes + marginAfter,
		first: marginBefore,
		last:  marginBefore + windowMinutes,
	}
	n := tl.n
	tl.samples = make([]int, n)
	tl.kinds = make([][numFocusKinds]int, n)
	tl.lastKind = make([]focusKind, n)
	tl.present = make([]bool, n)
	tl.lowSignal = make([]bool, n)
	tl.hbs = make([]int, n)
	tl.moved = make([]bool, n)
	tl.events = make([]int, n)
	tl.busy = make([]bool, n)
	tl.busyProj = make([]string, n)
	tl.projects = make([][]projectCount, n)
	tl.focusOK = make([]bool, n)
	tl.engaged = make([]bool, n)
	tl.sessionOf = map[string][]minuteIndex{}

	for _, s := range p.samples {
		if i, ok := tl.index(s.t); ok {
			tl.samples[i]++
			tl.kinds[i][s.kind]++
			tl.lastKind[i] = s.kind
		}
	}
	for _, h := range p.heartbeats {
		if i, ok := tl.index(h.t); ok {
			tl.hbs[i]++
			tl.events[i]++
			tl.moved[i] = tl.moved[i] || h.moved
		}
	}
	for _, e := range p.ai {
		if i, ok := tl.index(e.t); ok {
			tl.events[i]++
			if m := tl.sessionOf[e.session]; len(m) == 0 || m[len(m)-1] != i {
				tl.sessionOf[e.session] = append(m, i)
			}
		}
	}
	for _, e := range p.events {
		if i, ok := tl.index(e.t); ok {
			tl.projects[i] = addProjectCount(tl.projects[i], e.project)
		}
	}
	for i := range n {
		tl.lowSignal[i] = !tl.samplesWithin(i, int(lowSignalRadius/time.Minute))
		if tl.lowSignal[i] {
			tl.present[i] = tl.moved[i]
		} else {
			tl.present[i] = hasTimeIn(p.instants, tl.minute(i), tl.minute(i+1))
		}
	}
	return tl
}

// addProjectCount counts one event for project, keeping counts sorted by
// project name so output order never depends on event order.
func addProjectCount(counts []projectCount, project string) []projectCount {
	i, found := slices.BinarySearchFunc(counts, project, func(c projectCount, p string) int {
		switch {
		case c.project < p:
			return -1
		case c.project > p:
			return 1
		}
		return 0
	})
	if found {
		counts[i].count++
		return counts
	}
	return slices.Insert(counts, i, projectCount{project, 1})
}

// minute returns the start of minute i.
func (tl *timeline) minute(i int) time.Time {
	return tl.base.Add(time.Duration(i) * time.Minute)
}

// index returns the minute containing t, if it is on the timeline.
func (tl *timeline) index(t time.Time) (int, bool) {
	i := floorMinutes(t.Sub(tl.base))
	return i, i >= 0 && i < tl.n
}

// floorMinutes is d in whole minutes, rounded down (also for negative d).
func floorMinutes(d time.Duration) int {
	if d < 0 {
		return -int((-d + time.Minute - 1) / time.Minute)
	}
	return int(d / time.Minute)
}

// samplesWithin reports whether minutes i-r..i+r hold any activity sample.
func (tl *timeline) samplesWithin(i, r int) bool {
	for j := max(0, i-r); j <= min(tl.n-1, i+r); j++ {
		if tl.samples[j] > 0 {
			return true
		}
	}
	return false
}

// mark sets set[i] for every minute that the span [a, b] touches. A span
// ending exactly on a minute boundary doesn't touch the next minute; a
// zero-length span touches the minute it is in.
func (tl *timeline) mark(set []bool, a, b time.Time) {
	lo, hi := tl.touched(a, b)
	for i := lo; i < hi; i++ {
		set[i] = true
	}
}

// touched returns the minutes [lo, hi) that the span [a, b] touches, clipped
// to the timeline (see mark).
func (tl *timeline) touched(a, b time.Time) (lo, hi int) {
	if b.Before(a) {
		return 0, 0
	}
	lo = floorMinutes(a.Sub(tl.base))
	hi = floorMinutes(b.Sub(tl.base)) + 1
	if b.After(a) && tl.minute(hi-1).Equal(b) {
		hi--
	}
	lo, hi = max(lo, 0), min(hi, tl.n)
	return lo, max(lo, hi)
}

// markBusy records the minutes in which some agent was working: inside a
// turn and not waiting for permission. Turns come sorted by start, so the
// latest-starting busy turn names the minute's project.
func (tl *timeline) markBusy(turns []turn) {
	for _, t := range turns {
		for _, s := range t.busySpans() {
			lo, hi := tl.touched(s.from, s.to)
			for i := lo; i < hi; i++ {
				tl.busy[i] = true
				tl.busyProj[i] = t.project
			}
		}
	}
}

// activeSession reports an editor heartbeat or AI event in the 15 minutes
// before minute i, or in it.
func (tl *timeline) activeSession(i int) bool {
	for j := max(0, i-int(activeSessionWindow/time.Minute)); j <= i; j++ {
		if tl.events[j] > 0 {
			return true
		}
	}
	return false
}

// focusAllowed reports whether the focused app in minute i is somewhere work
// happens: an editor, terminal or AI app, a browser during an active session,
// or unknown (no focus signal is never held against the person). With no
// sample in the minute, the latest sample of the previous two minutes decides.
func (tl *timeline) focusAllowed(i int) bool {
	ok := func(k focusKind) bool {
		switch k {
		case focusBrowser:
			return tl.activeSession(i)
		case focusOther:
			return false
		}
		return true
	}
	if tl.samples[i] > 0 {
		for k := range numFocusKinds {
			if tl.kinds[i][k] > 0 && ok(k) {
				return true
			}
		}
		return false
	}
	for j := i - 1; j >= max(0, i-int(lowSignalRadius/time.Minute)); j-- {
		if tl.samples[j] > 0 {
			return ok(tl.lastKind[j])
		}
	}
	return true
}

// dominantFocus is the focus kind with the most samples in minute i (ties go
// to the earlier kind in focusKind order), or unknown with no samples.
func (tl *timeline) dominantFocus(i int) focusKind {
	best, bestN := focusUnknown, 0
	for k := range numFocusKinds {
		if tl.kinds[i][k] > bestN {
			best, bestN = k, tl.kinds[i][k]
		}
	}
	return best
}

// recentlyPresent reports input in minute i or one of the graceMinutes before.
func (tl *timeline) recentlyPresent(i int) bool {
	for j := max(0, i-graceMinutes); j <= i; j++ {
		if tl.present[j] {
			return true
		}
	}
	return false
}

// backgroundEditor reports a low-signal minute with heartbeats but no human
// write or cursor movement in it or the backgroundEditorMinutes-1 before it.
func (tl *timeline) backgroundEditor(i int) bool {
	if !tl.lowSignal[i] || tl.hbs[i] == 0 {
		return false
	}
	for j := max(0, i-backgroundEditorMinutes+1); j <= i; j++ {
		if tl.moved[j] {
			return false
		}
	}
	return true
}

// hasTimeIn reports whether sorted ts holds a time in [a, b).
func hasTimeIn(ts []time.Time, a, b time.Time) bool {
	i, _ := slices.BinarySearchFunc(ts, a, time.Time.Compare)
	return i < len(ts) && ts[i].Before(b)
}

// lastProject returns the project of the latest named event in [a, b), or "".
func lastProject(events []projectEvent, a, b time.Time) string {
	i, _ := slices.BinarySearchFunc(events, b, func(e projectEvent, t time.Time) int { return e.t.Compare(t) })
	if i == 0 || events[i-1].t.Before(a) {
		return ""
	}
	// Several events at the same instant: pick the greatest project name, so
	// the choice doesn't depend on anything but the data.
	return events[i-1].project
}
