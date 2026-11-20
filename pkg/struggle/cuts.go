package struggle

import (
	"slices"
	"time"
)

// promptsBySession groups the turns started by a prompt by session, in
// session order, each sorted by start.
func promptsBySession(turns []turn) [][]turn {
	bySession := map[string][]turn{}
	var sessions []string
	for _, t := range turns { // sorted by start
		if !t.prompt {
			continue
		}
		if _, ok := bySession[t.session]; !ok {
			sessions = append(sessions, t.session)
		}
		bySession[t.session] = append(bySession[t.session], t)
	}
	slices.Sort(sessions)
	out := make([][]turn, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, bySession[s])
	}
	return out
}

// shortPrompt reports a known prompt length of at most rubberStampMaxChars:
// an acknowledgement such as "yes" or "continue".
func shortPrompt(t turn) bool {
	return t.chars >= 1 && t.chars <= rubberStampMaxChars
}

// loopSpans returns the spans of turns cut by the loop rule: in one session,
// a prompt whose keyed hash was already sent at least loopRepeats-1 times in
// the loopWindow before it (so the 3rd and later repeats), with no human
// heartbeat since the earliest of those sends. Short prompts are left to the
// rubber-stamp rule, and prompts without a hash never match. The rule looks
// only backwards; the cut covers the turn's ruleSpan.
func loopSpans(turns []turn, heartbeats []heartbeat) []span {
	var out []span
	for _, ts := range promptsBySession(turns) {
		seen := map[string][]time.Time{}
		for _, t := range ts {
			if t.hash == "" || shortPrompt(t) {
				continue
			}
			// Keep only the sends still inside the window with no editing by
			// the person since, so memory stays small.
			recent := slices.DeleteFunc(seen[t.hash], func(at time.Time) bool {
				return t.start.Sub(at) > loopWindow || humanHeartbeatIn(heartbeats, at, t.start)
			})
			if len(recent)+1 >= loopRepeats {
				out = append(out, t.ruleSpan())
			}
			seen[t.hash] = append(recent, t.start)
		}
	}
	return out
}

// rubberStampSpans returns the spans cut by the rubber-stamp rule.
//
// A prompt is a stamp when it follows a turn_end of its session within
// rubberStampGap, is 1..rubberStampMaxChars characters long, and no human
// heartbeat came between the two. Three consecutive prompts of a session that
// are all stamps, the first and third at most rubberStampStreakWindow apart,
// make a streak; the turns of the second and third are cut, so every stamp
// after the first of a run is cut. The rule reads at most the streak window
// back, and the second stamp is cut only from lookaheadClip before the third
// arrives, so it never changes a minute the agent has already finalised.
func rubberStampSpans(turns []turn, heartbeats []heartbeat) []span {
	var out []span
	for _, ts := range promptsBySession(turns) {
		stamp := make([]bool, len(ts))
		for k, t := range ts {
			stamp[k] = isStamp(t, heartbeats)
		}
		streak := func(a, c int) bool {
			return a >= 0 && c < len(ts) && stamp[a] && stamp[a+1] && stamp[c] &&
				ts[c].start.Sub(ts[a].start) <= rubberStampStreakWindow
		}
		for k, t := range ts {
			s := t.ruleSpan()
			switch {
			case streak(k-2, k): // third or later stamp of a streak
				out = append(out, s)
			case streak(k-1, k+1): // second stamp, cut once the third arrives
				s.from = maxTime(s.from, ts[k+1].start.Add(-lookaheadClip))
				if !s.from.After(s.to) {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func isStamp(t turn, heartbeats []heartbeat) bool {
	gap := t.start.Sub(t.prevTurnEnd)
	return t.afterTurnEnd && gap >= 0 && gap <= rubberStampGap && shortPrompt(t) &&
		!humanHeartbeatIn(heartbeats, t.prevTurnEnd, t.start)
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

// markAutomated flags the turns started by a prompt with no OS input in the
// automatedLookback before it (e.g. /loop, scheduled tasks, SDK runs).
// Prompts on low-signal stretches (no activity samples near them) are not
// judged: missing data is not evidence of automation.
func markAutomated(turns []turn, samples []sample, instants []time.Time) {
	for k := range turns {
		t := &turns[k]
		if !t.prompt || !sampleIn(samples, t.start.Add(-lowSignalRadius), t.start.Add(lowSignalRadius)) {
			continue
		}
		t.automated = !hasTimeIn(instants, t.start.Add(-automatedLookback), t.start.Add(automatedTolerance))
	}
}

// automatedSpans returns the ruleSpans of the turns markAutomated flagged.
func automatedSpans(turns []turn) []span {
	var out []span
	for _, t := range turns {
		if t.automated {
			out = append(out, t.ruleSpan())
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
