package programapi

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// The text helpers below reproduce Hackatime's ApplicationHelper at 43bde10.

// shortTimeDetailed is short_time_detailed: "5h 10m 10s", "10m 10s", "10m";
// "" for zero.
func shortTimeDetailed(secs int64) string {
	h, m, s := secs/3600, secs%3600/60, secs%60
	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	if s > 0 {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}
	return strings.Join(parts, " ")
}

// shortTimeSimple is short_time_simple: "5h 10m", "10m", "0m".
func shortTimeSimple(secs int64) string {
	h, m := secs/3600, secs%3600/60
	if h == 0 && m == 0 {
		return "0m"
	}
	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	return strings.Join(parts, " ")
}

// digitalTime is digital_time: "HH:MM:SS".
func digitalTime(secs int64) string {
	return fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs%3600/60, secs%60)
}

// percent is (100.0 * part / total).round(2), and 0 when total is 0 (where
// Ruby would produce NaN and fail to render).
func percent(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(10000*float64(part)/float64(total)) / 100
}

// isoSeconds formats t as Hackatime's "%Y-%m-%dT%H:%M:%SZ" in UTC.
func isoSeconds(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// railsTime formats t as Rails renders a UTC time in JSON (millisecond
// precision): "2024-03-20T00:00:00.000Z".
func railsTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// unixSeconds is t as fractional Unix seconds, Hackatime's heartbeat time.
func unixSeconds(t time.Time) float64 {
	return float64(t.UnixMicro()) / 1e6
}
