package struggle

import (
	"math"
	"time"
)

// regularSpans finds runs of event times too regular for a human: scriptedRun
// consecutive intervals, each within [lo, hi], whose standard deviation is
// below scriptedMaxStdDev seconds. It returns the span of each such run
// (overlapping runs are returned separately; callers only mark minutes),
// reaching back at most finalityHorizon from the run's last event so a flag
// never lands on a minute that is already final. ts must be sorted.
func regularSpans(ts []time.Time, lo, hi time.Duration) []span {
	if len(ts) <= scriptedRun {
		return nil
	}
	gaps := make([]float64, len(ts)) // gaps[i] = ts[i] - ts[i-1], in seconds
	var out []span
	start := 0 // index of the first time in the current run of valid intervals
	for i := 1; i < len(ts); i++ {
		d := ts[i].Sub(ts[i-1])
		gaps[i] = d.Seconds()
		if d < lo || d > hi {
			start = i
			continue
		}
		if i-start >= scriptedRun && stdDev(gaps[i-scriptedRun+1:i+1]) < scriptedMaxStdDev {
			out = append(out, span{maxTime(ts[i-scriptedRun], ts[i].Add(-finalityHorizon)), ts[i]})
		}
	}
	return out
}

// stdDev is the population standard deviation of xs (two passes, so it stays
// exact for large means).
func stdDev(xs []float64) float64 {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var sq float64
	for _, x := range xs {
		sq += (x - mean) * (x - mean)
	}
	return math.Sqrt(sq / float64(len(xs)))
}

// scriptedSpans returns spans where input timing is too regular for a human:
//   - editor heartbeats at a fixed cadence faster than the WakaTime 2-minute
//     throttle (scriptedHBShortest..scriptedHBLongest);
//   - OS input instants at a fixed cadence (a mouse jiggler or key repeater).
//     Only instants followed by at least scriptedQuietIdle of quiet count, so
//     continuous typing, which the 5 s sampler sees at its own rhythm, can't
//     look periodic.
func scriptedSpans(hbs []heartbeat, samples []sample) []span {
	hbTimes := make([]time.Time, len(hbs))
	for i, h := range hbs {
		hbTimes[i] = h.t
	}
	out := regularSpans(hbTimes, scriptedHBShortest, scriptedHBLongest)
	return append(out, regularSpans(quietInstants(samples), scriptedInputShortest, scriptedInputLongest)...)
}

// quietInstants merges the input instants that several samples observed
// (within instantMergeGap of each other) and keeps those that some sample saw
// at least scriptedQuietIdle after. samples are sorted by time, so each new
// instant is at or after the previous ones except for sensor noise.
func quietInstants(samples []sample) []time.Time {
	var out []time.Time
	var cur time.Time
	var curQuiet, have bool
	flush := func() {
		if have && curQuiet {
			out = append(out, cur)
		}
	}
	for _, s := range samples {
		if !s.hasInput {
			continue
		}
		if have && absDuration(s.input.Sub(cur)) <= instantMergeGap {
			curQuiet = curQuiet || s.idle >= scriptedQuietIdle
			continue
		}
		flush()
		cur, curQuiet, have = s.input, s.idle >= scriptedQuietIdle, true
	}
	flush()
	return out
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
