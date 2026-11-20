package struggle

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// sample is an activity sample reduced to what the rules read.
type sample struct {
	t    time.Time
	kind focusKind
	// input is the instant of the last OS input this sample saw (t - idle);
	// hasInput is false when the idle reading was unusable (NaN, negative).
	input    time.Time
	idle     time.Duration
	hasInput bool
}

// heartbeat is an editor heartbeat with its project resolved.
type heartbeat struct {
	t       time.Time
	project string
	// moved: a write, a cursor or line change, or a switch to another file,
	// by the person (AI-attributed heartbeats never move).
	moved bool
	ai    bool
}

// aiEvent is an AI event with its project resolved.
type aiEvent struct {
	t       time.Time
	kind    wire.AIEventKind
	session string
	project string
	chars   int
	hash    string
}

// projectEvent is any event that names a project, for attribution.
type projectEvent struct {
	t       time.Time
	project string
}

// prepared holds sorted, de-duplicated, normalised copies of an Input.
type prepared struct {
	from, to time.Time // the window, whole minutes, UTC
	// horizon is the latest point the data speaks for: To, or a later event.
	horizon    time.Time
	tz         *time.Location
	samples    []sample
	instants   []time.Time // sorted OS input instants
	heartbeats []heartbeat
	ai         []aiEvent
	// events lists heartbeats and AI events that have a project, by time.
	events     []projectEvent
	activeTime []ActiveTime
}

func prepare(in Input) prepared {
	p := prepared{
		from: in.Window.From.UTC().Truncate(time.Minute),
		to:   in.Window.To.UTC().Truncate(time.Minute),
		tz:   in.TZ,
	}
	if p.to.Sub(p.from) > MaxWindow {
		p.to = p.from.Add(MaxWindow)
	}
	if p.tz == nil {
		p.tz = time.UTC
	}
	p.horizon = p.to
	repos := newRepoIndex(in.Repos)

	p.samples, p.instants = prepareSamples(in.Activity)
	p.heartbeats = prepareHeartbeats(in.Heartbeats, repos)
	p.ai = prepareAI(in.AI, repos)
	p.activeTime = prepareActiveTime(in.ActiveTime)

	if n := len(p.ai); n > 0 && p.ai[n-1].t.After(p.horizon) {
		p.horizon = p.ai[n-1].t
	}
	p.events = mergeProjectEvents(p.heartbeats, p.ai)
	return p
}

// mergeProjectEvents merges the time-sorted heartbeats and AI events that name
// a project into one list sorted by (time, project).
func mergeProjectEvents(hbs []heartbeat, ais []aiEvent) []projectEvent {
	out := make([]projectEvent, 0, len(hbs)+len(ais))
	for _, h := range hbs {
		if h.project != "" {
			out = append(out, projectEvent{h.t, h.project})
		}
	}
	mid := len(out)
	for _, e := range ais {
		if e.project != "" {
			out = append(out, projectEvent{e.t, e.project})
		}
	}
	// Both halves are sorted by time; ties need the project order too, so
	// finish with a sort that is linear on nearly sorted input.
	byTimeProject := func(a, b projectEvent) int {
		return cmp.Or(a.t.Compare(b.t), strings.Compare(a.project, b.project))
	}
	if mid > 0 && mid < len(out) {
		merged := make([]projectEvent, 0, len(out))
		i, j := 0, mid
		for i < mid && j < len(out) {
			if byTimeProject(out[i], out[j]) <= 0 {
				merged, i = append(merged, out[i]), i+1
			} else {
				merged, j = append(merged, out[j]), j+1
			}
		}
		merged = append(append(merged, out[i:mid]...), out[j:]...)
		out = merged
	}
	sortIfNeeded(out, byTimeProject)
	return out
}

// sortIfNeeded sorts s unless it already is: agents read events from SQLite
// in time order, and checking is linear while sorting is not.
func sortIfNeeded[T any](s []T, compare func(a, b T) int) {
	if !slices.IsSortedFunc(s, compare) {
		slices.SortFunc(s, compare)
	}
}

func prepareSamples(in []wire.ActivitySample) ([]sample, []time.Time) {
	raw := slices.Clone(in)
	sortIfNeeded(raw, func(a, b wire.ActivitySample) int {
		return cmp.Or(
			a.Time.Compare(b.Time),
			cmp.Compare(a.IdleSeconds, b.IdleSeconds),
			strings.Compare(a.FocusKind, b.FocusKind),
			strings.Compare(a.FocusApp, b.FocusApp),
		)
	})
	raw = slices.CompactFunc(raw, func(a, b wire.ActivitySample) bool {
		return a.Time.Equal(b.Time) && a.IdleSeconds == b.IdleSeconds &&
			a.FocusKind == b.FocusKind && a.FocusApp == b.FocusApp
	})
	out := make([]sample, 0, len(raw))
	instants := make([]time.Time, 0, len(raw))
	for _, r := range raw {
		s := sample{t: r.Time.UTC(), kind: parseFocusKind(r.FocusKind)}
		// A NaN, infinite or negative reading says nothing about input.
		if idle := r.IdleSeconds; idle >= 0 && !math.IsInf(idle, 1) {
			s.idle = time.Duration(min(idle, maxIdleSeconds) * float64(time.Second))
			s.input = s.t.Add(-s.idle)
			s.hasInput = true
			instants = append(instants, s.input)
		}
		out = append(out, s)
	}
	sortIfNeeded(instants, time.Time.Compare)
	return out, instants
}

func prepareHeartbeats(in []Heartbeat, repos repoIndex) []heartbeat {
	raw := slices.Clone(in)
	sortIfNeeded(raw, compareHeartbeats)
	raw = slices.CompactFunc(raw, func(a, b Heartbeat) bool { return compareHeartbeats(a, b) == 0 })
	out := make([]heartbeat, 0, len(raw))
	var prev *Heartbeat // previous human (non-AI) heartbeat
	for i := range raw {
		r := &raw[i]
		h := heartbeat{t: r.Time.UTC(), ai: r.AI, project: repos.project(r.Entity)}
		if h.project == "" {
			h.project = r.Project
		}
		if !r.AI {
			h.moved = r.IsWrite || (prev != nil && (prev.Entity != r.Entity ||
				intChanged(prev.Lineno, r.Lineno) || intChanged(prev.Cursorpos, r.Cursorpos)))
			prev = r
		}
		out = append(out, h)
	}
	return out
}

func compareHeartbeats(a, b Heartbeat) int {
	return cmp.Or(
		a.Time.Compare(b.Time),
		strings.Compare(a.Project, b.Project),
		strings.Compare(a.Entity, b.Entity),
		compareBool(a.IsWrite, b.IsWrite),
		compareIntPtr(a.Lineno, b.Lineno),
		compareIntPtr(a.Cursorpos, b.Cursorpos),
		compareBool(a.AI, b.AI),
	)
}

func prepareAI(in []wire.AIEvent, repos repoIndex) []aiEvent {
	raw := slices.Clone(in)
	sortIfNeeded(raw, compareAI)
	raw = slices.CompactFunc(raw, func(a, b wire.AIEvent) bool { return compareAI(a, b) == 0 })
	out := make([]aiEvent, 0, len(raw))
	for _, r := range raw {
		e := aiEvent{
			t: r.Time.UTC(), kind: r.Kind, session: string(r.Harness) + "/" + r.SessionID,
			project: repos.project(r.Cwd), chars: r.PromptChars, hash: r.PromptHash,
		}
		if e.project == "" {
			e.project = r.Project
		}
		out = append(out, e)
	}
	return out
}

// kindRank orders AI events that share a timestamp along a turn's lifecycle.
func kindRank(k wire.AIEventKind) int {
	switch k {
	case wire.AISessionStart:
		return 0
	case wire.AIPromptSubmit:
		return 1
	case wire.AIToolStart:
		return 2
	case wire.AIPermissionAsk:
		return 3
	case wire.AIToolEnd:
		return 4
	case wire.AITurnEnd:
		return 5
	case wire.AISessionEnd:
		return 6
	}
	return 7
}

func compareAI(a, b wire.AIEvent) int {
	return cmp.Or(
		a.Time.Compare(b.Time),
		cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)),
		strings.Compare(string(a.Kind), string(b.Kind)),
		strings.Compare(string(a.Harness), string(b.Harness)),
		strings.Compare(a.SessionID, b.SessionID),
		strings.Compare(a.Project, b.Project),
		strings.Compare(a.Cwd, b.Cwd),
		strings.Compare(a.Model, b.Model),
		strings.Compare(a.Tool, b.Tool),
		cmp.Compare(a.PromptChars, b.PromptChars),
		strings.Compare(a.PromptHash, b.PromptHash),
		strings.Compare(a.Decision, b.Decision),
	)
}

func prepareActiveTime(in []ActiveTime) []ActiveTime {
	out := slices.Clone(in)
	cmpAT := func(a, b ActiveTime) int {
		return cmp.Or(
			strings.Compare(a.SessionID, b.SessionID),
			a.From.Compare(b.From),
			a.To.Compare(b.To),
			cmp.Compare(a.UserSecs, b.UserSecs),
			cmp.Compare(a.CLISecs, b.CLISecs),
			strings.Compare(string(a.Harness), string(b.Harness)),
			strings.Compare(a.ProjectCwd, b.ProjectCwd),
		)
	}
	slices.SortFunc(out, cmpAT)
	return slices.CompactFunc(out, func(a, b ActiveTime) bool { return cmpAT(a, b) == 0 })
}

// intChanged reports a known change: both values present and different.
func intChanged(a, b *int) bool {
	return a != nil && b != nil && *a != *b
}

func compareIntPtr(a, b *int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return cmp.Compare(*a, *b)
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}
