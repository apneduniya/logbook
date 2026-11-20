package struggle

import (
	"slices"
	"time"
)

// loopSpans returns the spans of turns cut by the loop rule: in one session,
// a prompt whose keyed hash was already sent at least loopRepeats-1 times in
// the loopWindow before it (so the 3rd and later repeats). The rule looks
// only backwards. Prompts without a hash never match.
func loopSpans(turns []turn) []span {
	type key struct{ session, hash string }
	seen := map[key][]time.Time{}
	var out []span
	for _, t := range turns { // sorted by start
		if !t.prompt || t.hash == "" {
			continue
		}
		k := key{t.session, t.hash}
		// Keep only the sends still inside the window, so memory stays small.
		recent := slices.DeleteFunc(seen[k], func(at time.Time) bool { return t.start.Sub(at) > loopWindow })
		if len(recent)+1 >= loopRepeats {
			out = append(out, span{t.start, t.end})
		}
		seen[k] = append(recent, t.start)
	}
	return out
}

// rubberStampSpans returns the spans cut by the rubber-stamp rule.
//
// A prompt is a stamp when it follows the session's previous turn's turn_end
// within rubberStampGap, is 1..rubberStampMaxChars characters long, and no
// human heartbeat came between the two. rubberStampStreak or more stamps in a
// row form a streak; the turns of every stamp after the first are cut. The
// second stamp is cut only once the third arrives, and only as far back as
// finalityHorizon before it, so the rule never reaches past minutes the agent
// has already finalised.
func rubberStampSpans(turns []turn, heartbeats []heartbeat) []span {
	bySession := map[string][]turn{}
	var sessions []string
	for _, t := range turns {
		if _, ok := bySession[t.session]; !ok {
			sessions = append(sessions, t.session)
		}
		bySession[t.session] = append(bySession[t.session], t)
	}
	slices.Sort(sessions)
	var out []span
	for _, s := range sessions {
		ts := bySession[s]
		run := 0 // consecutive stamps so far
		for i := 1; i < len(ts); i++ {
			if !isStamp(ts[i-1], ts[i], heartbeats) {
				run = 0
				continue
			}
			run++
			switch {
			case run == rubberStampStreak:
				second := ts[i-1]
				from := maxTime(second.start, ts[i].start.Add(-finalityHorizon))
				out = append(out, span{from, second.end}, span{ts[i].start, ts[i].end})
			case run > rubberStampStreak:
				out = append(out, span{ts[i].start, ts[i].end})
			}
		}
	}
	return out
}

func isStamp(prev, t turn, heartbeats []heartbeat) bool {
	gap := t.start.Sub(prev.end)
	return t.prompt && prev.endedByTurnEnd &&
		gap >= 0 && gap <= rubberStampGap &&
		t.chars >= 1 && t.chars <= rubberStampMaxChars &&
		!humanHeartbeatIn(heartbeats, prev.end, t.start)
}

// humanHeartbeatIn reports a non-AI heartbeat in [a, b].
func humanHeartbeatIn(hbs []heartbeat, a, b time.Time) bool {
	i, _ := slices.BinarySearchFunc(hbs, a, func(h heartbeat, t time.Time) int { return h.t.Compare(t) })
	for ; i < len(hbs) && !hbs[i].t.After(b); i++ {
		if !hbs[i].ai {
			return true
		}
	}
	return false
}

// automatedSpans returns the turns started by a prompt with no OS input in
// the automatedLookback before it (e.g. /loop, scheduled tasks, SDK runs).
// Prompts on low-signal stretches (no activity samples near them) are not
// judged: missing data is not evidence of automation.
func automatedSpans(turns []turn, samples []sample, instants []time.Time) []span {
	var out []span
	for _, t := range turns {
		if !t.prompt {
			continue
		}
		if !sampleIn(samples, t.start.Add(-lowSignalRadius), t.start.Add(lowSignalRadius)) {
			continue
		}
		if !hasTimeIn(instants, t.start.Add(-automatedLookback), t.start.Add(automatedTolerance)) {
			out = append(out, span{t.start, t.end})
		}
	}
	return out
}

// sampleIn reports an activity sample in [a, b].
func sampleIn(samples []sample, a, b time.Time) bool {
	i, _ := slices.BinarySearchFunc(samples, a, func(s sample, t time.Time) int { return s.t.Compare(t) })
	return i < len(samples) && !samples[i].t.After(b)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
