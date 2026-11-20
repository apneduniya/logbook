package stats

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

func loc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSummaryAggregates(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	a := seedtest.User(t, st, "alice", "UTC", true)
	b := seedtest.User(t, st, "bob", "UTC", true)

	seedtest.Span(t, st, a, utc(2026, 11, 10, 9, 0), 30, seedtest.Minute{Project: "logbook", Language: "Go"})
	seedtest.Span(t, st, a, utc(2026, 11, 10, 10, 0), 10, seedtest.Minute{Project: "site", Language: "Svelte", Editor: "zed"})
	seedtest.Span(t, st, a, utc(2026, 11, 11, 9, 0), 5, seedtest.Minute{Project: "logbook", Label: "agent"})
	seedtest.Span(t, st, a, utc(2026, 11, 11, 9, 5), 2, seedtest.Minute{Project: "logbook", Label: "idle"})
	seedtest.Minutes(t, st, a, seedtest.Minute{At: utc(2026, 11, 12, 0, 0), Project: "logbook", Category: "debugging", Seconds: 42})
	// Outside the range and another user's data must not count.
	seedtest.Span(t, st, a, utc(2026, 11, 13, 0, 0), 10, seedtest.Minute{})
	seedtest.Span(t, st, b, utc(2026, 11, 10, 9, 0), 60, seedtest.Minute{Project: "logbook"})

	svc := NewService(st)
	sum, err := svc.Summary(ctx, a, utc(2026, 11, 10, 0, 0), utc(2026, 11, 13, 0, 0), time.UTC, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := int64(30*60 + 10*60 + 42)
	if sum.TotalSeconds != wantTotal {
		t.Errorf("TotalSeconds = %d, want %d", sum.TotalSeconds, wantTotal)
	}
	if sum.Labels != (Labels{You: wantTotal, Agent: 300, Idle: 120}) {
		t.Errorf("Labels = %+v", sum.Labels)
	}
	if want := []Item{{"logbook", 1842}, {"site", 600}}; !equalItems(projectItems(sum.Projects), want) {
		t.Errorf("Projects = %+v, want %+v", sum.Projects, want)
	}
	if want := []Item{{"Go", 1842}, {"Svelte", 600}}; !equalItems(sum.Languages, want) {
		t.Errorf("Languages = %+v, want %+v", sum.Languages, want)
	}
	if want := []Item{{"vscode", 1842}, {"zed", 600}}; !equalItems(sum.Editors, want) {
		t.Errorf("Editors = %+v, want %+v", sum.Editors, want)
	}
	if want := []Item{{"coding", 2400}, {"debugging", 42}}; !equalItems(sum.Categories, want) {
		t.Errorf("Categories = %+v, want %+v", sum.Categories, want)
	}
	wantDays := []Day{{Date: "2026-11-10", Seconds: 2400}, {Date: "2026-11-11"}, {Date: "2026-11-12", Seconds: 42}}
	if len(sum.Days) != len(wantDays) {
		t.Fatalf("Days = %+v, want %+v", sum.Days, wantDays)
	}
	for i := range wantDays {
		if sum.Days[i] != wantDays[i] {
			t.Errorf("Days[%d] = %+v, want %+v", i, sum.Days[i], wantDays[i])
		}
	}

	site := "site"
	filtered, err := svc.Summary(ctx, a, utc(2026, 11, 10, 0, 0), utc(2026, 11, 13, 0, 0), time.UTC, Filter{Project: &site})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.TotalSeconds != 600 || len(filtered.Projects) != 1 {
		t.Errorf("filtered summary = %+v", filtered)
	}
}

// projectItems drops the credited field, for comparing raw totals.
func projectItems(ps []ProjectItem) []Item {
	out := make([]Item, len(ps))
	for i, p := range ps {
		out[i] = Item{Name: p.Name, Seconds: p.Seconds}
	}
	return out
}

func equalItems(a, b []Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSummaryTimezones(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	u := seedtest.User(t, st, "tz", "UTC", true)
	ny := loc(t, "America/New_York")
	kol := loc(t, "Asia/Kolkata")

	// 2026-11-01 is the US fall-back day: 01:30 local happens twice.
	seedtest.Span(t, st, u, utc(2026, 11, 1, 5, 30), 1, seedtest.Minute{})   // 01:30 EDT
	seedtest.Span(t, st, u, utc(2026, 11, 1, 6, 30), 1, seedtest.Minute{})   // 01:30 EST
	seedtest.Span(t, st, u, utc(2026, 11, 2, 4, 59), 1, seedtest.Minute{})   // 23:59 EST on Nov 1
	seedtest.Span(t, st, u, utc(2026, 11, 2, 5, 0), 1, seedtest.Minute{})    // 00:00 EST on Nov 2
	seedtest.Span(t, st, u, utc(2026, 3, 8, 6, 59), 1, seedtest.Minute{})    // 01:59 EST, spring-forward day
	seedtest.Span(t, st, u, utc(2026, 3, 8, 7, 0), 1, seedtest.Minute{})     // 03:00 EDT, same day
	seedtest.Span(t, st, u, utc(2026, 11, 20, 18, 29), 2, seedtest.Minute{}) // 23:59 and 00:00 IST

	tests := []struct {
		name       string
		tz         *time.Location
		start, end string
		wantDays   map[string]int64
	}{
		{"fall back day is 25h", ny, "2026-11-01", "2026-11-02", map[string]int64{"2026-11-01": 180, "2026-11-02": 60}},
		{"spring forward day", ny, "2026-03-08", "2026-03-08", map[string]int64{"2026-03-08": 120}},
		{"kolkata midnight split", kol, "2026-11-20", "2026-11-21", map[string]int64{"2026-11-20": 60, "2026-11-21": 60}},
		{"utc buckets differ", time.UTC, "2026-11-01", "2026-11-02", map[string]int64{"2026-11-01": 120, "2026-11-02": 120}},
	}
	svc := NewService(st)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, e := mustDate(t, tt.start), mustDate(t, tt.end)
			sum, err := svc.Summary(ctx, u, s.Start(tt.tz), e.AddDays(1).Start(tt.tz), tt.tz, Filter{})
			if err != nil {
				t.Fatal(err)
			}
			var total int64
			for _, d := range sum.Days {
				if d.Seconds != tt.wantDays[d.Date] {
					t.Errorf("day %s = %d, want %d", d.Date, d.Seconds, tt.wantDays[d.Date])
				}
				total += d.Seconds
			}
			if total != sum.TotalSeconds {
				t.Errorf("days add up to %d, total is %d", total, sum.TotalSeconds)
			}
		})
	}
}

func TestSummaryRangeValidation(t *testing.T) {
	svc := NewService(storetest.New(t))
	ctx := context.Background()
	start := utc(2025, 1, 1, 0, 0)
	tests := []struct {
		name    string
		from    time.Time
		to      time.Time
		wantErr bool
	}{
		{"one day", start, start.AddDate(0, 0, 1), false},
		{"366 days", start, start.AddDate(0, 0, 366), false},
		{"367 days", start, start.AddDate(0, 0, 367), true},
		{"empty", start, start, true},
		{"reversed", start.AddDate(0, 0, 1), start, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Summary(ctx, uuid.New(), tt.from, tt.to, time.UTC, Filter{})
			if got := errors.Is(err, ErrInvalidRange); got != tt.wantErr {
				t.Fatalf("Summary() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestStreakFromMinutes(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	u := seedtest.User(t, st, "streaky", "Asia/Kolkata", true)
	kol := loc(t, "Asia/Kolkata")
	day := func(s string, h int) time.Time { return mustDate(t, s).Start(kol).Add(time.Duration(h) * time.Hour) }

	seedtest.Span(t, st, u, day("2026-11-10", 10), 15, seedtest.Minute{})               // qualifies
	seedtest.Span(t, st, u, day("2026-11-11", 10), 15, seedtest.Minute{})               // qualifies
	seedtest.Span(t, st, u, day("2026-11-12", 10), 15, seedtest.Minute{})               // qualifies
	seedtest.Span(t, st, u, day("2026-11-18", 10), 14, seedtest.Minute{})               // 14 min: no
	seedtest.Span(t, st, u, day("2026-11-18", 11), 30, seedtest.Minute{Label: "agent"}) // agent time doesn't count
	seedtest.Span(t, st, u, day("2026-11-19", 23), 8, seedtest.Minute{Project: "a"})    // split across
	seedtest.Span(t, st, u, day("2026-11-19", 23).Add(8*time.Minute), 7, seedtest.Minute{Project: "b"})
	seedtest.Span(t, st, u, day("2026-11-20", 0), 20, seedtest.Minute{}) // today, after IST midnight

	svc := NewService(st)
	svc.now = func() time.Time { return day("2026-11-20", 12) }
	got, err := svc.Streak(ctx, u, kol)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Streak{Current: 2, Longest: 3}); got != want {
		t.Fatalf("Streak() = %+v, want %+v", got, want)
	}

	today, err := svc.Today(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if today.Start != "2026-11-20" || today.TotalSeconds != 20*60 || today.Streak.Current != 2 || today.Timezone != "Asia/Kolkata" {
		t.Fatalf("Today() = %+v", today)
	}
}

func TestHandlerSummary(t *testing.T) {
	st := storetest.New(t)
	a := seedtest.User(t, st, "alice", "UTC", true)
	b := seedtest.User(t, st, "bob", "UTC", true)
	seedtest.Span(t, st, a, utc(2026, 11, 10, 9, 0), 30, seedtest.Minute{Project: "secret"})

	svc := NewService(st)
	svc.now = func() time.Time { return utc(2026, 11, 12, 12, 0) }
	mux := http.NewServeMux()
	NewHandler(svc, httpapitest.StubSession).Register(mux)

	tests := []struct {
		name       string
		user       uuid.UUID
		target     string
		wantStatus int
		wantTotal  int64
		wantDays   int
	}{
		{"owner default range", a, "/api/v1/me/summary", 200, 1800, 7},
		{"owner explicit range", a, "/api/v1/me/summary?start=2026-11-10&end=2026-11-10", 200, 1800, 1},
		{"owner project filter", a, "/api/v1/me/summary?start=2026-11-10&end=2026-11-10&project=other", 200, 0, 1},
		{"other user sees nothing", b, "/api/v1/me/summary?start=2026-11-10&end=2026-11-10", 200, 0, 1},
		{"other user filter on owner project", b, "/api/v1/me/summary?start=2026-11-10&end=2026-11-10&project=secret", 200, 0, 1},
		{"unauthenticated", uuid.Nil, "/api/v1/me/summary", 401, 0, 0},
		{"bad start", a, "/api/v1/me/summary?start=nope", 400, 0, 0},
		{"bad end", a, "/api/v1/me/summary?end=2026-02-30", 400, 0, 0},
		{"reversed", a, "/api/v1/me/summary?start=2026-11-11&end=2026-11-10", 400, 0, 0},
		{"too long", a, "/api/v1/me/summary?start=2025-01-01&end=2026-01-02", 400, 0, 0},
		{"max length", a, "/api/v1/me/summary?start=2025-01-01&end=2026-01-01", 200, 0, 366},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httpapitest.Request(t, http.MethodGet, tt.target, tt.user, ""))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if rec.Code != 200 {
				return
			}
			var sum Summary
			if err := json.NewDecoder(rec.Body).Decode(&sum); err != nil {
				t.Fatal(err)
			}
			if sum.TotalSeconds != tt.wantTotal || len(sum.Days) != tt.wantDays {
				t.Fatalf("total = %d, days = %d; want %d, %d", sum.TotalSeconds, len(sum.Days), tt.wantTotal, tt.wantDays)
			}
		})
	}
}

func TestHandlerTodayIsolation(t *testing.T) {
	st := storetest.New(t)
	a := seedtest.User(t, st, "alice", "UTC", true)
	b := seedtest.User(t, st, "bob", "UTC", true)
	now := utc(2026, 11, 12, 12, 0)
	seedtest.Span(t, st, a, utc(2026, 11, 12, 9, 0), 20, seedtest.Minute{})

	svc := NewService(st)
	svc.now = func() time.Time { return now }
	mux := http.NewServeMux()
	NewHandler(svc, httpapitest.StubSession).Register(mux)

	for _, tc := range []struct {
		user       uuid.UUID
		wantTotal  int64
		wantStreak int
	}{{a, 1200, 1}, {b, 0, 0}} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httpapitest.Request(t, http.MethodGet, "/api/v1/me/today", tc.user, ""))
		if rec.Code != 200 {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		var got Today
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.TotalSeconds != tc.wantTotal || got.Streak.Current != tc.wantStreak {
			t.Errorf("user %v: today = %+v", tc.user, got)
		}
	}
}

func TestLocationFallsBackToUTC(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"", "Local", "Not/AZone"} {
		if got := LoadLocation(ctx, name); got != time.UTC {
			t.Errorf("LoadLocation(%q) = %v, want UTC", name, got)
		}
	}
	if got := LoadLocation(ctx, "Europe/Berlin"); got.String() != "Europe/Berlin" {
		t.Errorf("LoadLocation(Europe/Berlin) = %v", got)
	}
}
