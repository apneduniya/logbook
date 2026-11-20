package stats

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	_ "time/tzdata" // day buckets depend on IANA zones even where the host has no zoneinfo

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// MaxRangeDays is the longest range, in local days, a summary may cover.
const MaxRangeDays = 366

// ErrInvalidRange is returned for empty, reversed or too-long ranges.
var ErrInvalidRange = errors.New("invalid range")

// Querier is the subset of the generated queries the service needs.
type Querier interface {
	StatsUserTimezone(ctx context.Context, id uuid.UUID) (string, error)
	StatsBreakdown(ctx context.Context, arg db.StatsBreakdownParams) ([]db.StatsBreakdownRow, error)
	StatsQualifyingDays(ctx context.Context, arg db.StatsQualifyingDaysParams) ([]string, error)
}

// Service computes aggregates for one user at a time.
type Service struct {
	q   Querier
	now func() time.Time
}

// NewService returns a Service reading from q.
func NewService(q Querier) *Service {
	return &Service{q: q, now: time.Now}
}

// Filter narrows a summary. A nil Project means all projects.
type Filter struct {
	Project *string
}

// Item is one row of a breakdown, such as a project or a language.
type Item struct {
	Name    string `json:"name"`
	Seconds int64  `json:"seconds"`
}

// Day is the credited time on one local date.
type Day struct {
	Date    string `json:"date"`
	Seconds int64  `json:"seconds"`
}

// Labels splits all tracked time by minute label.
type Labels struct {
	You   int64 `json:"you"`
	Agent int64 `json:"agent"`
	Idle  int64 `json:"idle"`
}

// Summary is the user's activity over a range of local days.
type Summary struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Timezone string `json:"timezone"`
	// TotalSeconds is credited ("you") time; it equals Labels.You.
	TotalSeconds int64  `json:"total_seconds"`
	Labels       Labels `json:"labels"`
	Projects     []Item `json:"projects"`
	Languages    []Item `json:"languages"`
	Editors      []Item `json:"editors"`
	Categories   []Item `json:"categories"`
	// Days has one entry per local date in the range, zero-filled.
	Days []Day `json:"days"`
}

// Today is today's summary plus the user's streak.
type Today struct {
	Summary
	Streak Streak `json:"streak"`
}

// Location returns the user's configured time zone, falling back to UTC
// when the stored value is not a valid IANA name.
func (s *Service) Location(ctx context.Context, userID uuid.UUID) (*time.Location, error) {
	name, err := s.q.StatsUserTimezone(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("loading user timezone: %w", err)
	}
	return LoadLocation(ctx, name), nil
}

// LoadLocation resolves an IANA zone name, falling back to UTC (and logging)
// when it is invalid. "Local" is rejected because it depends on the host.
func LoadLocation(ctx context.Context, name string) *time.Location {
	if name != "" && name != "Local" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	slog.WarnContext(ctx, "invalid user timezone, using UTC", "timezone", name)
	return time.UTC
}

// Now returns the service clock's current time.
func (s *Service) Now() time.Time { return s.now() }

// Summary aggregates credited time in [from, to) for userID, bucketing days
// in tz. The range must cover between 1 and MaxRangeDays local days.
func (s *Service) Summary(ctx context.Context, userID uuid.UUID, from, to time.Time, tz *time.Location, f Filter) (Summary, error) {
	if !from.Before(to) {
		return Summary{}, fmt.Errorf("%w: start must be before end", ErrInvalidRange)
	}
	first, last := DateOf(from, tz), DateOf(to.Add(-time.Nanosecond), tz)
	if n := first.DaysUntil(last) + 1; n > MaxRangeDays {
		return Summary{}, fmt.Errorf("%w: %d days is longer than %d", ErrInvalidRange, n, MaxRangeDays)
	}
	var project pgtype.Text
	if f.Project != nil {
		project = pgtype.Text{String: *f.Project, Valid: true}
	}
	rows, err := s.q.StatsBreakdown(ctx, db.StatsBreakdownParams{
		Tz: tz.String(), UserID: userID, FromTime: from.UTC(), ToTime: to.UTC(), Project: project,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("querying breakdown: %w", err)
	}
	return buildSummary(rows, first, last, tz), nil
}

// buildSummary turns breakdown rows into a sorted, zero-filled Summary.
func buildSummary(rows []db.StatsBreakdownRow, first, last Date, tz *time.Location) Summary {
	sum := Summary{
		Start: first.String(), End: last.String(), Timezone: tz.String(),
		Projects: []Item{}, Languages: []Item{}, Editors: []Item{}, Categories: []Item{},
	}
	perDay := map[string]int64{}
	for _, r := range rows {
		it := Item{Name: r.Key, Seconds: r.Seconds}
		switch r.Dimension {
		case "label":
			switch r.Key {
			case "you":
				sum.Labels.You = r.Seconds
			case "agent":
				sum.Labels.Agent = r.Seconds
			case "idle":
				sum.Labels.Idle = r.Seconds
			}
		case "project":
			sum.Projects = append(sum.Projects, it)
		case "language":
			sum.Languages = append(sum.Languages, it)
		case "editor":
			sum.Editors = append(sum.Editors, it)
		case "category":
			sum.Categories = append(sum.Categories, it)
		case "day":
			perDay[r.Key] = r.Seconds
		}
	}
	sum.TotalSeconds = sum.Labels.You
	for _, items := range [][]Item{sum.Projects, sum.Languages, sum.Editors, sum.Categories} {
		SortItems(items)
	}
	for d := first; !last.Before(d); d = d.AddDays(1) {
		key := d.String()
		sum.Days = append(sum.Days, Day{Date: key, Seconds: perDay[key]})
	}
	return sum
}

// SortItems orders items by seconds, largest first, then by name.
func SortItems(items []Item) {
	slices.SortFunc(items, func(a, b Item) int {
		if c := cmp.Compare(b.Seconds, a.Seconds); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}

// Streak returns the user's current and longest streak as of today in tz.
func (s *Service) Streak(ctx context.Context, userID uuid.UUID, tz *time.Location) (Streak, error) {
	raw, err := s.q.StatsQualifyingDays(ctx, db.StatsQualifyingDaysParams{
		Tz: tz.String(), UserID: userID, MinSeconds: StreakMinSeconds,
	})
	if err != nil {
		return Streak{}, fmt.Errorf("querying qualifying days: %w", err)
	}
	days := make([]Date, 0, len(raw))
	for _, r := range raw {
		d, err := ParseDate(r)
		if err != nil {
			return Streak{}, fmt.Errorf("reading qualifying day: %w", err)
		}
		days = append(days, d)
	}
	return computeStreak(days, DateOf(s.now(), tz)), nil
}

// Today returns today's summary in the user's time zone and their streak.
func (s *Service) Today(ctx context.Context, userID uuid.UUID) (Today, error) {
	tz, err := s.Location(ctx, userID)
	if err != nil {
		return Today{}, err
	}
	today := DateOf(s.now(), tz)
	sum, err := s.Summary(ctx, userID, today.Start(tz), today.AddDays(1).Start(tz), tz, Filter{})
	if err != nil {
		return Today{}, err
	}
	streak, err := s.Streak(ctx, userID, tz)
	if err != nil {
		return Today{}, err
	}
	return Today{Summary: sum, Streak: streak}, nil
}
