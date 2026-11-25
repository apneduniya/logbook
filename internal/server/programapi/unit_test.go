package programapi

import (
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

func TestMaskTrust(t *testing.T) {
	tests := []struct {
		stored string
		linked bool
		want   Trust
	}{
		{"", false, Trust{Level: "blue", Value: 0}},
		{"unknown", true, Trust{Level: "blue", Value: 0}},
		{"blue", true, Trust{Level: "blue", Value: 0, Verified: true}},
		{"green", true, Trust{Level: "green", Value: 2, Verified: true}},
		{"yellow", true, Trust{Level: "blue", Value: 0, Verified: true}},
		{"red", true, Trust{Level: "red", Value: 1, Verified: true}},
		{"purple", true, Trust{Level: "blue", Value: 0}},
	}
	for _, tc := range tests {
		if got := MaskTrust(tc.stored, tc.linked); got != tc.want {
			t.Errorf("MaskTrust(%q, %v) = %+v, want %+v", tc.stored, tc.linked, got, tc.want)
		}
	}
	if !MaskTrust("red", true).Banned() || MaskTrust("yellow", true).Banned() {
		t.Error("Banned() is wrong")
	}
}

func TestParseTime(t *testing.T) {
	utc := func(y int, m time.Month, d, h, mi, s int) time.Time { return time.Date(y, m, d, h, mi, s, 0, time.UTC) }
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2025-12-15", utc(2025, 12, 15, 0, 0, 0), true},
		{"2025-12-15T10:20:30Z", utc(2025, 12, 15, 10, 20, 30), true},
		{"2025-12-15T10:20:30+05:30", utc(2025, 12, 15, 4, 50, 30), true},
		{"2025-12-15T10:20:30", utc(2025, 12, 15, 10, 20, 30), true},
		{"2025-12-15 10:20:30", utc(2025, 12, 15, 10, 20, 30), true},
		{"2025-12-15T10:20", utc(2025, 12, 15, 10, 20, 0), true},
		{"  2025-12-15  ", utc(2025, 12, 15, 0, 0, 0), true},
		{"yesterday", time.Time{}, false},
		{"2025-13-01", time.Time{}, false},
		{"", time.Time{}, false},
	}
	for _, tc := range tests {
		got, ok := ParseTime(tc.in)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("ParseTime(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestListParam(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		{"", nil},
		{"filter_by_project=", nil},
		{"filter_by_project=a", []string{"a"}},
		{"filter_by_project=a,b", []string{"a", "b"}},
		{"filter_by_project=a,,b,,", []string{"a", "", "b"}},
		{"filter_by_project[]=a&filter_by_project[]=b", []string{"a", "b"}},
		{"filter_by_project[]=a&filter_by_project[]=,&filter_by_project[]=b", []string{"a", "b"}},
		{"filter_by_project=x&filter_by_project[]=a", []string{"x"}},
	}
	for _, tc := range tests {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		if got := listParam(q, "filter_by_project"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("listParam(%q) = %#v, want %#v", tc.query, got, tc.want)
		}
	}
}

func TestClassifyIdent(t *testing.T) {
	tests := []struct {
		id      string
		allowMy bool
		want    identKind
	}{
		{"my", true, identMy},
		{"my", false, identUsername},
		{"100000001", true, identNumeric},
		{"007", true, identNumeric},
		{"orpheus", true, identUsername},
		{"Orpheus-2", true, identUsername},
		{"", true, identInvalid},
		{"a", true, identInvalid},
		{"a.b", true, identInvalid},
		{"user@example.com", true, identInvalid},
	}
	for _, tc := range tests {
		if got := classifyIdent(tc.id, tc.allowMy); got != tc.want {
			t.Errorf("classifyIdent(%q, %v) = %v, want %v", tc.id, tc.allowMy, got, tc.want)
		}
	}
}

func TestRubyToI(t *testing.T) {
	for in, want := range map[string]int{"": 0, "5": 5, "12abc": 12, "abc": 0, "-3": -3, " 7": 7, "+2": 2} {
		if got := rubyToI(in); got != want {
			t.Errorf("rubyToI(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestHackatimeTextHelpers(t *testing.T) {
	tests := []struct {
		secs                   int64
		detailed, simple, digi string
	}{
		{0, "", "0m", "00:00:00"},
		{59, "59s", "0m", "00:00:59"},
		{600, "10m", "10m", "00:10:00"},
		{3600 + 600 + 10, "1h 10m 10s", "1h 10m", "01:10:10"},
		{5 * 3600, "5h", "5h", "05:00:00"},
	}
	for _, tc := range tests {
		if got := shortTimeDetailed(tc.secs); got != tc.detailed {
			t.Errorf("shortTimeDetailed(%d) = %q, want %q", tc.secs, got, tc.detailed)
		}
		if got := shortTimeSimple(tc.secs); got != tc.simple {
			t.Errorf("shortTimeSimple(%d) = %q, want %q", tc.secs, got, tc.simple)
		}
		if got := digitalTime(tc.secs); got != tc.digi {
			t.Errorf("digitalTime(%d) = %q, want %q", tc.secs, got, tc.digi)
		}
	}
	if got := percent(1, 3); got != 33.33 {
		t.Errorf("percent(1, 3) = %v", got)
	}
	if got := percent(5, 0); got != 0 {
		t.Errorf("percent(5, 0) = %v", got)
	}
}

// TestSpansFromRuns pins Hackatime's to_span: every span but the last gets
// a capped two-minute tail, and a last span of zero length is dropped.
func TestSpansFromRuns(t *testing.T) {
	runs := []db.ProgramHeartbeatRunsRow{
		{FirstTime: 1000, LastTime: 1100.4},
		{FirstTime: 2000, LastTime: 2000},
		{FirstTime: 3000, LastTime: 3000},
	}
	want := []Span{
		{StartTime: 1000, EndTime: 1220.4, Duration: 220},
		{StartTime: 2000, EndTime: 2120, Duration: 120},
	}
	if got := spansFromRuns(runs); !reflect.DeepEqual(got, want) {
		t.Fatalf("spansFromRuns = %+v, want %+v", got, want)
	}
	if got := spansFromRuns(nil); len(got) != 0 {
		t.Fatalf("no runs = %+v", got)
	}
}

func TestParseStatsRequest(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC)
	q, _ := url.ParseQuery("")
	req, err := ParseStatsRequest(q, now)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Languages || req.Projects || !req.Range.From.Equal(now.AddDate(-10, 0, 0)) ||
		!req.Range.To.Equal(time.Date(2026, 7, 1, 23, 59, 59, 0, time.UTC)) {
		t.Fatalf("defaults = %+v", req)
	}
	q, _ = url.ParseQuery("features=&limit=-4")
	if req, _ = ParseStatsRequest(q, now); req.Languages || req.Projects || req.Limit != 0 {
		t.Fatalf("empty features = %+v, want no breakdowns and no limit", req)
	}
	q, _ = url.ParseQuery("features=projects,editors&total_seconds=true&test_param=true&no_ai_coding=true&raw=true")
	req, _ = ParseStatsRequest(q, now)
	if req.Languages || !req.Projects || !req.ProjectsFeature || !req.TotalOnly || !req.Test || !req.NoAICoding || !req.Raw {
		t.Fatalf("flags = %+v", req)
	}
	for _, bad := range []string{"start_date=nope", "end_date=2025-02-30"} {
		q, _ = url.ParseQuery(bad)
		if _, err := ParseStatsRequest(q, now); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
