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
	hash := make([]byte, 32)
	_, _ = rand.Read(hash) // crypto/rand.Read never returns an error
	if err := st.SeedTestHeartbeat(context.Background(), db.SeedTestHeartbeatParams{
		UserID: user, Time: at.UTC(), FieldsHash: hash, Entity: entity,
		Category: category, Project: project, Language: "Go", Editor: "vscode",
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
