package programapi

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
)

// rawHeartbeat stores one heartbeat exactly as given: unlike
// seedtest.Heartbeats, an empty project stays empty, as ingest stores it.
func rawHeartbeat(t *testing.T, st *store.Store, user uuid.UUID, at time.Time, project, category, entity string) {
	t.Helper()
	rawHeartbeatIn(t, st, user, at, project, "Go", category, entity)
}

// rawHeartbeatIn is rawHeartbeat with a language.
func rawHeartbeatIn(t *testing.T, st *store.Store, user uuid.UUID, at time.Time, project, language, category, entity string) {
	t.Helper()
	hash := make([]byte, 32)
	_, _ = rand.Read(hash) // crypto/rand.Read never returns an error
	if err := st.SeedTestHeartbeat(context.Background(), db.SeedTestHeartbeatParams{
		UserID: user, Time: at.UTC(), FieldsHash: hash, Entity: entity,
		Category: category, Project: project, Language: language, Editor: "vscode",
	}); err != nil {
		t.Fatalf("seeding heartbeat %v: %v", at, err)
	}
}

func (f *fixture) getJSON(t *testing.T, c call) map[string]any {
	t.Helper()
	rec := f.do(t, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", c.target, rec.Code, rec.Body)
	}
	return decode(t, rec)
}

// TestHiddenUnknownProject: heartbeats without a project are stored with an
// empty project and reported as "Unknown". Hiding "Unknown" must hide those
// heartbeats too, not only their totals: before, the heartbeat queries
// compared the hidden name with the stored empty string and listed their
// file paths.
func TestHiddenUnknownProject(t *testing.T) {
	f := newFixture(t)
	u := seedtest.User(t, f.st, "noproj", "UTC", true)
	const key = "44444444-4444-4444-8444-444444444444"
	f.auth.keys[key] = u
	for i := range 3 {
		rawHeartbeat(t, f.st, u, day(9, i), "", "coding", "/home/noproj/private/diary.txt")
	}
	seedtest.Span(t, f.st, u, day(9, 0), 2, seedtest.Minute{Project: "Unknown"})
	seedtest.ProjectSettings(t, f.st, u, "Unknown", "", true, false)

	for _, c := range []call{
		{target: "/api/v1/my/heartbeats?start_time=2026-06-10T00:00:00Z&end_time=2026-06-10T23:59:59Z", bearer: key},
		{target: "/api/v1/users/noproj/heartbeats/spans?start_date=2026-06-10&end_date=2026-06-11"},
		{target: "/api/v1/users/noproj/stats?features=projects,languages&start_date=2026-06-01"},
		{target: "/api/v1/users/noproj/stats?features=projects&filter_by_project=Unknown&raw=true&start_date=2026-06-01"},
		{target: "/api/v1/users/noproj/projects/details"},
	} {
		rec := f.do(t, c)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.target, rec.Code, rec.Body)
		}
		if body := rec.Body.String(); strings.Contains(body, "diary.txt") || strings.Contains(body, "Unknown") {
			t.Errorf("%s leaks the hidden Unknown project: %s", c.target, body)
		}
	}
	spans := f.getJSON(t, call{target: "/api/v1/users/noproj/heartbeats/spans?start_date=2026-06-10&end_date=2026-06-11"})
	if got := spans["spans"].([]any); len(got) != 0 {
		t.Errorf("spans = %v, want none", got)
	}
	for _, target := range []string{
		"/api/v1/users/noproj/stats?total_seconds=true&raw=true&start_date=2026-06-01",
		"/api/v1/users/noproj/stats?total_seconds=true&raw=true&filter_by_project=Unknown&start_date=2026-06-01",
		"/api/v1/users/noproj/stats?total_seconds=true&raw=true&filter_by_category=coding&start_date=2026-06-01",
	} {
		if got := f.getJSON(t, call{target: target}); got["raw_seconds"] != float64(0) {
			t.Errorf("%s = %v, want 0 raw seconds", target, got)
		}
	}
}

// TestFilteredRawTotalsMatchHackatime: Hackatime filters heartbeats first
// and then takes LEAST(gap to the previous one, 120 s), so with alpha at
// 09:00 and 09:02 and beta at 09:01, alpha alone is 120 s. Before, Logbook
// filtered the rollup, where alpha only held the 60 s after beta (60).
func TestFilteredRawTotalsMatchHackatime(t *testing.T) {
	f := newFixture(t)
	u := seedtest.User(t, f.st, "hopper", "UTC", true)
	at := func(h, m, s int) time.Time { return time.Date(2026, 6, 10, h, m, s, 0, time.UTC) }
	rawHeartbeat(t, f.st, u, at(9, 0, 0), "alpha", "coding", "a.go")
	rawHeartbeat(t, f.st, u, at(9, 1, 0), "beta", "browsing", "b.go")
	rawHeartbeat(t, f.st, u, at(9, 2, 0), "alpha", "coding", "a.go")
	// The rollup of those heartbeats: beta credited 09:00-09:01, alpha 09:01-09:02.
	seedtest.Span(t, f.st, u, at(9, 0, 0), 1, seedtest.Minute{Project: "beta", Category: "browsing"})
	seedtest.Span(t, f.st, u, at(9, 1, 0), 1, seedtest.Minute{Project: "alpha"})
	// Boundary case, for test_param: alpha at 10:59:00, beta at 10:59:30,
	// alpha at 11:00:40 and 11:01:00; the range starts at 11:00.
	rawHeartbeat(t, f.st, u, at(10, 59, 0), "alpha", "coding", "a.go")
	rawHeartbeat(t, f.st, u, at(10, 59, 30), "beta", "coding", "b.go")
	rawHeartbeat(t, f.st, u, at(11, 0, 40), "alpha", "coding", "a.go")
	rawHeartbeat(t, f.st, u, at(11, 1, 0), "alpha", "coding", "a.go")

	const morning = "&start_date=2026-06-10T09:00:00Z&end_date=2026-06-10T10:00:00Z"
	const late = "&start_date=2026-06-10T11:00:00Z&end_date=2026-06-10T12:00:00Z"
	for _, tc := range []struct {
		query string
		key   string
		want  float64
	}{
		{"total_seconds=true&raw=true&filter_by_project=alpha" + morning, "total_seconds", 120},
		{"total_seconds=true&filter_by_project=alpha" + morning, "raw_seconds", 120},
		{"total_seconds=true&raw=true&filter_by_category=coding" + morning, "total_seconds", 120},
		{"total_seconds=true&raw=true&filter_by_project=beta" + morning, "total_seconds", 0},
		{"total_seconds=true&raw=true" + morning, "total_seconds", 120}, // unfiltered: the rollup
		// test_param excludes browsing; Hackatime's boundary-aware total
		// also credits the first heartbeat its gap from the last one before
		// the range (11:00:40 - 10:59:00 = 100 s, capped at 120).
		{"total_seconds=true&raw=true&test_param=true&filter_by_project=alpha" + late, "total_seconds", 120},
		{"total_seconds=true&raw=true&filter_by_project=alpha" + late, "total_seconds", 20},
	} {
		got := f.getJSON(t, call{target: "/api/v1/users/hopper/stats?" + tc.query})
		if got[tc.key] != tc.want {
			t.Errorf("%s: %s = %v, want %v (%v)", tc.query, tc.key, got[tc.key], tc.want, got)
		}
	}
	// The summary's total and its breakdowns come from the same filtered timeline.
	body := f.getJSON(t, call{target: "/api/v1/users/hopper/stats?features=projects,languages&raw=true&filter_by_project=alpha" + morning})
	data := body["data"].(map[string]any)
	if data["total_seconds"] != float64(120) || data["raw_seconds"] != float64(120) {
		t.Errorf("summary = %v, want 120 raw", data)
	}
	projects := data["projects"].([]any)
	if len(projects) != 1 || projects[0].(map[string]any)["raw_seconds"] != float64(120) {
		t.Errorf("projects = %v, want alpha with 120 raw seconds", projects)
	}
}

// TestFilteredRawRowsRoundOnce: a filtered request's breakdown entries keep
// fractional seconds until each entry is summed, then round as Hackatime
// rounds (half to even). One project with a 0.4 s gap in each of two
// languages is 0.8 s, so 1 s, like the total; rounding each (project,
// language) piece first made the project 0.
func TestFilteredRawRowsRoundOnce(t *testing.T) {
	f := newFixture(t)
	u := seedtest.User(t, f.st, "babbage", "UTC", true)
	t0 := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	rawHeartbeatIn(t, f.st, u, t0, "alpha", "Go", "coding", "a.go")
	rawHeartbeatIn(t, f.st, u, t0.Add(400*time.Millisecond), "alpha", "Rust", "coding", "a.rs")
	rawHeartbeatIn(t, f.st, u, t0.Add(800*time.Millisecond), "alpha", "Go", "coding", "a.go")

	body := f.getJSON(t, call{target: "/api/v1/users/babbage/stats?features=projects,languages&raw=true&filter_by_project=alpha" +
		"&start_date=2026-06-10T09:00:00Z&end_date=2026-06-10T10:00:00Z"})
	data := body["data"].(map[string]any)
	if data["total_seconds"] != float64(1) {
		t.Errorf("total_seconds = %v, want 1", data["total_seconds"])
	}
	projects := data["projects"].([]any)
	if len(projects) != 1 || projects[0].(map[string]any)["raw_seconds"] != float64(1) {
		t.Errorf("projects = %v, want alpha with 1 raw second", projects)
	}
	for _, l := range data["languages"].([]any) {
		if s := l.(map[string]any)["total_seconds"]; s != float64(0) {
			t.Errorf("language %v = %v s, want 0 (0.4 s each)", l.(map[string]any)["name"], s)
		}
	}
}

// TestRawUniqueTotalSecondsMatchesHackatime:Hackatime's
// unique_heartbeat_seconds sums only the gaps of at most 120 s between
// consecutive coding heartbeats of the filtered projects, and truncates.
// Two heartbeats ten minutes apart are 0; before, Logbook read the rollup's
// two-minute credit (120).
func TestRawUniqueTotalSecondsMatchesHackatime(t *testing.T) {
	f := newFixture(t)
	u := seedtest.User(t, f.st, "lovelace", "UTC", true)
	at := func(h, m int, s float64) time.Time {
		return time.Date(2026, 6, 10, h, m, 0, 0, time.UTC).Add(time.Duration(s * float64(time.Second)))
	}
	rawHeartbeat(t, f.st, u, at(9, 0, 0), "alpha", "coding", "a.go")
	rawHeartbeat(t, f.st, u, at(9, 10, 0), "alpha", "coding", "a.go")
	seedtest.Span(t, f.st, u, at(9, 8, 0), 2, seedtest.Minute{Project: "alpha"})
	// 30.5 s and 30.6 s gaps, with a browsing heartbeat between that
	// Hackatime's coding_only leaves out: 61.1 s, truncated to 61.
	rawHeartbeat(t, f.st, u, at(12, 0, 0), "alpha", "coding", "a.go")
	rawHeartbeat(t, f.st, u, at(12, 0, 10), "alpha", "browsing", "a.go")
	rawHeartbeat(t, f.st, u, at(12, 0, 30.5), "alpha", "coding", "a.go")
	rawHeartbeat(t, f.st, u, at(12, 1, 1.1), "alpha", "coding", "a.go")

	for _, tc := range []struct {
		rng  string
		want float64
	}{
		{"&start_date=2026-06-10T09:00:00Z&end_date=2026-06-10T10:00:00Z", 0},
		{"&start_date=2026-06-10T12:00:00Z&end_date=2026-06-10T13:00:00Z", 61},
	} {
		body := f.getJSON(t, call{target: "/api/v1/users/lovelace/stats?features=projects&filter_by_project=alpha&raw=true" + tc.rng})
		if got := body["data"].(map[string]any)["unique_total_seconds"]; got != tc.want {
			t.Errorf("%s: unique_total_seconds = %v, want %v", tc.rng, got, tc.want)
		}
	}
}
