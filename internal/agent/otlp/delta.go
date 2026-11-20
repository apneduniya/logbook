package otlp

import "time"

// Interval is a point converted to an increment: Secs seconds during
// [From, To).
type Interval struct {
	From, To time.Time
	Secs     float64
}

// Last is the latest stored point of a cumulative series: its end time and
// running total.
type Last struct {
	To    time.Time
	Total float64
}

// Plausibility limits for a point, relative to the receiver's clock.
const (
	// maxFuture tolerates small clock differences; Claude Code stamps
	// points with this machine's clock.
	maxFuture = 2 * time.Minute
	// maxAge drops points older than local retention.
	maxAge = 90 * 24 * time.Hour
	// slack is how far an increment may exceed its interval's length
	// (export timing jitter). Active time of one type can't exceed wall time.
	slack = time.Minute
)

// DeltaInterval turns a delta datapoint into an interval. A point with no
// start time is a zero-length interval at Time. ok is false for a zero
// increment (nothing to record) or an implausible point.
func DeltaInterval(p Point, now time.Time) (Interval, bool) {
	from := p.Start
	if from.IsZero() {
		from = p.Time
	}
	iv := Interval{From: from, To: p.Time, Secs: p.Value}
	return iv, iv.Secs > 0 && plausible(iv, now)
}

// CumulativeInterval turns a cumulative datapoint into an increment since
// the series' previous point. last is the series' latest stored point (ok
// false when there is none). The rules:
//
//   - first point of a series: the whole total, over [Start, Time) (or a
//     zero-length interval at Time when Start is unset);
//   - a point no newer than last: a retry or reordering, skipped;
//   - a total below last's: the counter was reset (Claude Code restarted
//     the series without a new start time), so the whole new total counts,
//     over [last.To, Time);
//   - otherwise: the increase since last, over [last.To, Time).
//
// Zero increments are kept (emit is true), so the next increment covers only
// the time since this point. emit is false for a skipped or implausible
// point.
func CumulativeInterval(p Point, last Last, hasLast bool, now time.Time) (iv Interval, emit bool) {
	switch {
	case !hasLast:
		from := p.Start
		if from.IsZero() {
			from = p.Time
		}
		iv = Interval{From: from, To: p.Time, Secs: p.Value}
	case !p.Time.After(last.To):
		return Interval{}, false
	case p.Value < last.Total:
		iv = Interval{From: last.To, To: p.Time, Secs: p.Value}
	default:
		iv = Interval{From: last.To, To: p.Time, Secs: p.Value - last.Total}
	}
	return iv, plausible(iv, now)
}

// plausible reports whether an interval could be real: not in the future,
// not older than retention, and no more seconds than its length allows.
func plausible(iv Interval, now time.Time) bool {
	if iv.To.After(now.Add(maxFuture)) || iv.To.Before(now.Add(-maxAge)) || iv.To.Before(iv.From) {
		return false
	}
	return iv.Secs <= (iv.To.Sub(iv.From) + slack).Seconds()
}
