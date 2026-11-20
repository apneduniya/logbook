package otlp

import (
	"testing"
	"time"
)

func TestDeltaInterval(t *testing.T) {
	tests := []struct {
		name string
		p    Point
		want Interval
		ok   bool
	}{
		{"normal", Point{Start: minute(-2), Time: minute(-1), Value: 30}, Interval{minute(-2), minute(-1), 30}, true},
		{"no start", Point{Time: minute(-1), Value: 3}, Interval{minute(-1), minute(-1), 3}, true},
		{"zero", Point{Start: minute(-2), Time: minute(-1)}, Interval{minute(-2), minute(-1), 0}, false},
		{"more than wall time", Point{Start: minute(-2), Time: minute(-1), Value: 121}, Interval{minute(-2), minute(-1), 121}, false},
		{"within slack", Point{Start: minute(-2), Time: minute(-1), Value: 120}, Interval{minute(-2), minute(-1), 120}, true},
		{"future", Point{Start: minute(2), Time: minute(3), Value: 1}, Interval{minute(2), minute(3), 1}, false},
		{"slightly future", Point{Start: minute(0), Time: minute(1), Value: 1}, Interval{minute(0), minute(1), 1}, true},
		{"too old", Point{Start: now.Add(-91 * 24 * time.Hour), Time: now.Add(-91*24*time.Hour + time.Minute), Value: 1},
			Interval{now.Add(-91 * 24 * time.Hour), now.Add(-91*24*time.Hour + time.Minute), 1}, false},
		{"start in 1970", Point{Start: time.Unix(0, 1).UTC(), Time: minute(-1), Value: 1e9}, Interval{time.Unix(0, 1).UTC(), minute(-1), 1e9}, false},
		{"start before retention", Point{Start: now.Add(-91 * 24 * time.Hour), Time: minute(-1), Value: 60}, Interval{now.Add(-91 * 24 * time.Hour), minute(-1), 60}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DeltaInterval(tc.p, now)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("DeltaInterval = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestCumulativeInterval(t *testing.T) {
	last := Last{To: minute(-3), Total: 100}
	tests := []struct {
		name    string
		p       Point
		last    Last
		hasLast bool
		want    Interval
		emit    bool
	}{
		{"first point counts from start", Point{Start: minute(-60), Time: minute(-3), Value: 100}, Last{}, false,
			Interval{minute(-60), minute(-3), 100}, true},
		{"first point without start", Point{Time: minute(-3), Value: 0}, Last{}, false,
			Interval{minute(-3), minute(-3), 0}, true},
		{"increase", Point{Start: minute(-60), Time: minute(-2), Value: 130}, last, true,
			Interval{minute(-3), minute(-2), 30}, true},
		{"unchanged is a zero increment", Point{Start: minute(-60), Time: minute(-2), Value: 100}, last, true,
			Interval{minute(-3), minute(-2), 0}, true},
		{"reset counts the new total", Point{Start: minute(-60), Time: minute(-2), Value: 10}, last, true,
			Interval{minute(-3), minute(-2), 10}, true},
		{"retry of last point", Point{Start: minute(-60), Time: minute(-3), Value: 100}, last, true, Interval{}, false},
		{"older point", Point{Start: minute(-60), Time: minute(-4), Value: 90}, last, true, Interval{}, false},
		{"first point starting in 1970", Point{Start: time.Unix(0, 1).UTC(), Time: minute(-3), Value: 1e9}, Last{}, false,
			Interval{time.Unix(0, 1).UTC(), minute(-3), 1e9}, false},
		{"implausible jump", Point{Start: minute(-60), Time: minute(-2), Value: 300}, last, true,
			Interval{minute(-3), minute(-2), 200}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, emit := CumulativeInterval(tc.p, tc.last, tc.hasLast, now)
			if got != tc.want || emit != tc.emit {
				t.Fatalf("CumulativeInterval = %+v, %v; want %+v, %v", got, emit, tc.want, tc.emit)
			}
		})
	}
}
