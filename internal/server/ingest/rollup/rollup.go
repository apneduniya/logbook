package rollup

import (
	"cmp"
	"math"
	"slices"
	"sort"
	"time"
)

// Timeout is the longest gap between two heartbeats that still counts as
// continuous activity.
const Timeout = 2 * time.Minute

// UnknownProject is the rollup project name for heartbeats without a project.
const UnknownProject = "Unknown"

// maxSecondsPerMinute is the most a single minute can hold across all groups.
const maxSecondsPerMinute = 60

// Key is the set of attributes a rollup row is grouped by.
type Key struct {
	Project  string
	Language string
	Editor   string
	Category string
}

func (k Key) compare(o Key) int {
	return cmp.Or(
		cmp.Compare(k.Project, o.Project),
		cmp.Compare(k.Language, o.Language),
		cmp.Compare(k.Editor, o.Editor),
		cmp.Compare(k.Category, o.Category),
	)
}

// Heartbeat is the part of a heartbeat that rollups need.
type Heartbeat struct {
	Time time.Time
	Key  Key
}

// Row is one user_minutes row: seconds credited to Key during Minute.
type Row struct {
	Minute  time.Time
	Key     Key
	Seconds int16
}

// Range is the half-open time interval [Start, End).
type Range struct {
	Start time.Time
	End   time.Time
}

// Contains reports whether t lies in [Start, End).
func (r Range) Contains(t time.Time) bool {
	return !t.Before(r.Start) && t.Before(r.End)
}

// Windows returns the sorted, merged, minute-aligned windows whose rollup rows
// can change when heartbeats at the given times are added. Windows whose read
// ranges would overlap are merged, so the read ranges of the result are
// disjoint. It returns nil for no times.
func Windows(times []time.Time) []Range {
	if len(times) == 0 {
		return nil
	}
	sorted := slices.Clone(times)
	slices.SortFunc(sorted, func(a, b time.Time) int { return a.Compare(b) })

	var out []Range
	for _, t := range sorted {
		w := Range{
			Start: t.Add(-Timeout).UTC().Truncate(time.Minute),
			End:   t.Add(Timeout).UTC().Truncate(time.Minute).Add(time.Minute),
		}
		if n := len(out); n > 0 && !w.Start.After(out[n-1].End.Add(2*Timeout)) {
			if w.End.After(out[n-1].End) {
				out[n-1].End = w.End
			}
			continue
		}
		out = append(out, w)
	}
	return out
}

// ReadRange returns the inclusive interval of heartbeats needed to recompute
// window w: every heartbeat whose credit can fall inside w, plus the next
// heartbeat of each.
func ReadRange(w Range) (from, to time.Time) {
	return w.Start.Add(-Timeout), w.End.Add(Timeout)
}

// Compute returns the rollup rows inside windows for heartbeats, which must
// be sorted by time (ties in a stable, deterministic order) and must include
// every stored heartbeat in each window's ReadRange. windows must be sorted
// and disjoint, as returned by Windows. Rows are sorted by minute, then key,
// and rows with zero seconds are omitted.
func Compute(hbs []Heartbeat, windows []Range) []Row {
	if len(windows) == 0 {
		return nil
	}
	// Exact integer durations, so the result doesn't depend on summation order.
	credit := map[time.Time]map[Key]time.Duration{}
	for i := 0; i+1 < len(hbs); i++ {
		start, end := hbs[i].Time, hbs[i+1].Time
		gap := end.Sub(start)
		if gap <= 0 || gap > Timeout {
			continue
		}
		key := normalizeKey(hbs[i].Key)
		for m := start.UTC().Truncate(time.Minute); m.Before(end); m = m.Add(time.Minute) {
			if !inWindows(windows, m) {
				continue
			}
			lo, hi := maxTime(start, m), minTime(end, m.Add(time.Minute))
			if hi.After(lo) {
				if credit[m] == nil {
					credit[m] = map[Key]time.Duration{}
				}
				credit[m][key] += hi.Sub(lo)
			}
		}
	}

	minutes := make([]time.Time, 0, len(credit))
	for m := range credit {
		minutes = append(minutes, m)
	}
	slices.SortFunc(minutes, func(a, b time.Time) int { return a.Compare(b) })

	var rows []Row
	for _, m := range minutes {
		rows = append(rows, roundMinute(m, credit[m])...)
	}
	return rows
}

// roundMinute converts one minute's exact credits into whole seconds that add
// up to at most 60, scaling down proportionally if the total is over the cap
// and distributing rounding with the largest-remainder method.
func roundMinute(minute time.Time, groups map[Key]time.Duration) []Row {
	keys := make([]Key, 0, len(groups))
	var total time.Duration
	for k, d := range groups {
		keys = append(keys, k)
		total += d
	}
	slices.SortFunc(keys, Key.compare)

	scale := 1.0
	if total > maxSecondsPerMinute*time.Second {
		scale = float64(maxSecondsPerMinute*time.Second) / float64(total)
	}
	target := int(math.Round(min(total.Seconds()*scale, maxSecondsPerMinute)))

	type share struct {
		key   Key
		whole int
		frac  float64
	}
	shares := make([]share, len(keys))
	assigned := 0
	for i, k := range keys {
		exact := groups[k].Seconds() * scale
		whole := int(math.Floor(exact))
		shares[i] = share{key: k, whole: whole, frac: exact - float64(whole)}
		assigned += whole
	}
	order := make([]int, len(shares))
	for i := range order {
		order[i] = i
	}
	// Largest fraction first; ties keep key order so the result is deterministic.
	sort.SliceStable(order, func(a, b int) bool { return shares[order[a]].frac > shares[order[b]].frac })
	for i := 0; assigned < target && i < len(order); i++ {
		shares[order[i]].whole++
		assigned++
	}

	rows := make([]Row, 0, len(shares))
	for _, s := range shares {
		if s.whole > 0 {
			rows = append(rows, Row{Minute: minute, Key: s.key, Seconds: int16(s.whole)}) //nolint:gosec // s.whole <= 60
		}
	}
	return rows
}

func normalizeKey(k Key) Key {
	if k.Project == "" {
		k.Project = UnknownProject
	}
	return k
}

func inWindows(windows []Range, t time.Time) bool {
	i := sort.Search(len(windows), func(i int) bool { return windows[i].End.After(t) })
	return i < len(windows) && windows[i].Contains(t)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
