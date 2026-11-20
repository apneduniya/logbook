package stats

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

// countingQuerier counts streak queries and can fail the partial one.
type countingQuerier struct {
	Querier
	mu          sync.Mutex
	full        int
	between     int
	failBetween bool
}

func (c *countingQuerier) StatsQualifyingDays(ctx context.Context, arg db.StatsQualifyingDaysParams) ([]string, error) {
	c.mu.Lock()
	c.full++
	c.mu.Unlock()
	return c.Querier.StatsQualifyingDays(ctx, arg)
}

func (c *countingQuerier) StatsQualifyingDaysBetween(ctx context.Context, arg db.StatsQualifyingDaysBetweenParams) ([]string, error) {
	c.mu.Lock()
	c.between++
	fail := c.failBetween
	c.mu.Unlock()
	if fail {
		return nil, errors.New("boom")
	}
	return c.Querier.StatsQualifyingDaysBetween(ctx, arg)
}

func (c *countingQuerier) counts() (full, between int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.full, c.between
}

func TestStreakCacheRefreshesOnlyChangedDays(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	u := seedtest.User(t, st, "streaky", "UTC", true)
	other := seedtest.User(t, st, "other", "UTC", true)
	seedtest.Span(t, st, u, utc(2026, 11, 10, 9, 0), 15, seedtest.Minute{})
	seedtest.Span(t, st, u, utc(2026, 11, 11, 9, 0), 15, seedtest.Minute{})

	q := &countingQuerier{Querier: st}
	svc := NewService(q)
	now := utc(2026, 11, 12, 12, 0)
	svc.now = func() time.Time { return now }
	streak := func(tz *time.Location) Streak {
		t.Helper()
		s, err := svc.Streak(ctx, u, tz)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	expect := func(step string, got, want Streak, wantFull, wantBetween int) {
		t.Helper()
		full, between := q.counts()
		if got != want || full != wantFull || between != wantBetween {
			t.Fatalf("%s: streak %+v (want %+v), queries full=%d between=%d (want %d, %d)",
				step, got, want, full, between, wantFull, wantBetween)
		}
	}

	expect("first read loads all history", streak(time.UTC), Streak{Current: 2, Longest: 2}, 1, 0)
	expect("second read is cached", streak(time.UTC), Streak{Current: 2, Longest: 2}, 1, 0)

	seedtest.Span(t, st, u, utc(2026, 11, 12, 9, 0), 15, seedtest.Minute{})
	expect("unannounced write is not seen", streak(time.UTC), Streak{Current: 2, Longest: 2}, 1, 0)

	svc.MinutesChanged(other, utc(2026, 11, 12, 9, 0), utc(2026, 11, 12, 9, 15))
	expect("another user's change costs nothing", streak(time.UTC), Streak{Current: 2, Longest: 2}, 1, 0)

	svc.MinutesChanged(u, utc(2026, 11, 12, 9, 0), utc(2026, 11, 12, 9, 15))
	expect("announced change re-queries its day", streak(time.UTC), Streak{Current: 3, Longest: 3}, 1, 1)

	// A failed partial refresh keeps the range dirty for the next read.
	seedtest.Span(t, st, u, utc(2026, 11, 9, 9, 0), 15, seedtest.Minute{})
	svc.MinutesChanged(u, utc(2026, 11, 9, 9, 0), utc(2026, 11, 9, 9, 15))
	q.failBetween = true
	if _, err := svc.Streak(ctx, u, time.UTC); err == nil {
		t.Fatal("expected error from failed refresh")
	}
	q.failBetween = false
	expect("retry after failure", streak(time.UTC), Streak{Current: 4, Longest: 4}, 1, 3)

	expect("a new time zone reloads", streak(loc(t, "America/Los_Angeles")), Streak{Current: 4, Longest: 4}, 2, 3)
	now = now.Add(streakCacheTTL)
	expect("the TTL forces a reload", streak(loc(t, "America/Los_Angeles")), Streak{Current: 4, Longest: 4}, 3, 3)
}

func TestStreakCacheStoreRangeReplacesDays(t *testing.T) {
	tests := []struct {
		name        string
		cached      []string
		first, last string
		fresh       []string
		want        []string
	}{
		{"add a day", []string{"2026-11-10"}, "2026-11-11", "2026-11-11", []string{"2026-11-11"}, []string{"2026-11-10", "2026-11-11"}},
		{"day no longer qualifies", []string{"2026-11-10", "2026-11-11", "2026-11-12"}, "2026-11-11", "2026-11-11", nil, []string{"2026-11-10", "2026-11-12"}},
		{"range in the past", []string{"2026-11-10", "2026-11-20"}, "2025-01-01", "2025-01-03", []string{"2025-01-02"}, []string{"2025-01-02", "2026-11-10", "2026-11-20"}},
		{"unchanged", []string{"2026-11-10"}, "2026-11-10", "2026-11-10", []string{"2026-11-10"}, []string{"2026-11-10"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newStreakCache()
			e := c.entry(uuid.New())
			c.storeFull(e, "UTC", dates(t, tt.cached...), time.Now())
			c.storeRange(e, mustDate(t, tt.first), mustDate(t, tt.last), dates(t, tt.fresh...))
			got := c.snapshot(e)
			if len(got) != len(tt.want) {
				t.Fatalf("days = %v, want %v", got, tt.want)
			}
			for i, d := range got {
				if d.String() != tt.want[i] {
					t.Fatalf("days = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestStreakCacheConcurrentUse(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	u := seedtest.User(t, st, "busy", "UTC", true)
	seedtest.Span(t, st, u, utc(2026, 11, 11, 9, 0), 15, seedtest.Minute{})
	svc := NewService(st)
	svc.now = func() time.Time { return utc(2026, 11, 12, 12, 0) }
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 10 {
				svc.MinutesChanged(u, utc(2026, 11, 11, 9, i), utc(2026, 11, 11, 9, i+1))
				if _, err := svc.Streak(ctx, u, time.UTC); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	got, err := svc.Streak(ctx, u, time.UTC)
	if err != nil || got != (Streak{Current: 1, Longest: 1}) {
		t.Fatalf("Streak() = %+v, %v", got, err)
	}
}
