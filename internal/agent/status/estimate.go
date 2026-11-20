package status

import (
	"fmt"
	"strings"
	"time"
)

// MaxGap is the longest gap between heartbeats still counted as work. It
// matches the server's rule (internal/server/ingest/rollup).
const MaxGap = 2 * time.Minute

// CodingTime estimates coding time from heartbeat times sorted oldest
// first: each heartbeat is credited the gap to the next one, up to MaxGap.
// The server's rollups are authoritative; this is only the offline estimate.
func CodingTime(times []time.Time) time.Duration {
	var total time.Duration
	for i := 1; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		if gap > 0 && gap <= MaxGap {
			total += gap
		}
	}
	return total
}

// DurationText formats seconds like WakaTime: "2 hrs 17 mins", "1 hr",
// "49 mins", "7 secs", "0 secs".
func DurationText(sec int64) string {
	h, m, s := sec/3600, sec%3600/60, sec%60
	if h == 0 && m == 0 {
		return plural(s, "sec")
	}
	var parts []string
	if h > 0 {
		parts = append(parts, plural(h, "hr"))
	}
	if m > 0 {
		parts = append(parts, plural(m, "min"))
	}
	return strings.Join(parts, " ")
}

func plural(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
