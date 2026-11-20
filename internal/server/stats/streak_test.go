package stats

import (
	"testing"
	"time"
)

func mustDate(t *testing.T, s string) Date {
	t.Helper()
	d, err := ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func dates(t *testing.T, ss ...string) []Date {
	t.Helper()
	out := make([]Date, len(ss))
	for i, s := range ss {
		out[i] = mustDate(t, s)
	}
	return out
}

func TestComputeStreak(t *testing.T) {
	tests := []struct {
		name  string
		days  []string
		today string
		want  Streak
	}{
		{"no days", nil, "2026-11-20", Streak{}},
		{"only today", []string{"2026-11-20"}, "2026-11-20", Streak{1, 1}},
		{"ends yesterday still current", []string{"2026-11-18", "2026-11-19"}, "2026-11-20", Streak{2, 2}},
		{"ended two days ago", []string{"2026-11-17", "2026-11-18"}, "2026-11-20", Streak{0, 2}},
		{"gap resets run", []string{"2026-11-10", "2026-11-11", "2026-11-12", "2026-11-19", "2026-11-20"}, "2026-11-20", Streak{2, 3}},
		{"across month and year", []string{"2025-12-30", "2025-12-31", "2026-01-01"}, "2026-01-01", Streak{3, 3}},
		{"leap day", []string{"2028-02-28", "2028-02-29", "2028-03-01"}, "2028-03-02", Streak{3, 3}},
		{"future day ignored", []string{"2026-11-19", "2026-11-20", "2026-11-21"}, "2026-11-20", Streak{2, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeStreak(dates(t, tt.days...), mustDate(t, tt.today))
			if got != tt.want {
				t.Fatalf("computeStreak() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDateOfAndStart(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	kol, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		at   time.Time
		loc  *time.Location
		want string
	}{
		{"utc", time.Date(2026, 11, 20, 23, 59, 0, 0, time.UTC), time.UTC, "2026-11-20"},
		{"kolkata ahead", time.Date(2026, 11, 20, 18, 30, 0, 0, time.UTC), kol, "2026-11-21"},
		{"new york behind", time.Date(2026, 11, 21, 4, 59, 0, 0, time.UTC), ny, "2026-11-20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DateOf(tt.at, tt.loc).String(); got != tt.want {
				t.Fatalf("DateOf() = %s, want %s", got, tt.want)
			}
		})
	}
	// The fall-back day in New York is 25 hours long.
	d := mustDate(t, "2026-11-01")
	if got := d.AddDays(1).Start(ny).Sub(d.Start(ny)); got != 25*time.Hour {
		t.Fatalf("2026-11-01 in New York lasts %v, want 25h", got)
	}
}

func TestParseDateRejectsBadInput(t *testing.T) {
	for _, s := range []string{"", "2026-13-01", "2026-02-30", "20261120", "2026-11-20T00:00:00Z", "2026-1-2"} {
		if _, err := ParseDate(s); err == nil {
			t.Errorf("ParseDate(%q) succeeded", s)
		}
	}
}
