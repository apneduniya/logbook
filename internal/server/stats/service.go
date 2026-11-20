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
	StatsQualifyingDaysBetween(ctx context.Context, arg db.StatsQualifyingDaysBetweenParams) ([]string, error)
	StatsUserHasDevices(ctx context.Context, userID uuid.UUID) (bool, error)
	StatsCreditedBreakdown(ctx context.Context, arg db.StatsCreditedBreakdownParams) ([]db.StatsCreditedBreakdownRow, error)
}

// Service computes aggregates for one user at a time.
type Service struct {
	q       Querier
	now     func() time.Time
	streaks *streakCache
}

// NewService returns a Service reading from q. Wire it into ingest's
// Publisher (see live.Fanout) so streaks pick up new minutes at once.
func NewService(q Querier) *Service {
	return &Service{q: q, now: time.Now, streaks: newStreakCache()}
}

// MinutesChanged records that userID's minutes in [from, to) changed, so the
// next streak read re-queries those days. It does no I/O.
func (s *Service) MinutesChanged(userID uuid.UUID, from, to time.Time) {
	s.streaks.invalidate(userID, from.UTC(), to.UTC())
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

// Day is the time on one local date.
type Day struct {
	Date    string `json:"date"`
	Seconds int64  `json:"seconds"`
	// CreditedSeconds is the agent's credited time (see Summary).
	CreditedSeconds *int64 `json:"credited_seconds"`
}

// ProjectItem is one project's raw time and, for users with a device, its
// credited time. A project can have credited time and no raw time (the
// agent saw AI work but no editor heartbeats), or the other way round.
type ProjectItem struct {
	Name            string `json:"name"`
	Seconds         int64  `json:"seconds"`
	CreditedSeconds *int64 `json:"credited_seconds"`
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

	// CreditedSeconds is the struggle engine's credited time from the
	// user's devices (ADR 0010, 0014): per minute, the sum over devices
	// and projects, capped at 60 s. The Credited* fields and
	// MultiDeviceMinutes are null when the user has never registered a
	// device, so a UI can say "no evidence" rather than "0".
	CreditedSeconds *int64 `json:"credited_seconds"`
	// CreditedLabels splits the minutes the devices reported by label, 60 s
	// per minute, each minute counted once under its strongest label (you,
	// then agent, then idle).
	CreditedLabels *Labels `json:"credited_labels"`
	// MultiDeviceMinutes counts minutes in which two or more devices credited
	// "you" time (the multi_device flag); they are capped, not added.
	MultiDeviceMinutes *int64 `json:"multi_device_minutes"`

	Projects   []ProjectItem `json:"projects"`
	Languages  []Item        `json:"languages"`
	Editors    []Item        `json:"editors"`
	Categories []Item        `json:"categories"`
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
	hasDevices, err := s.q.StatsUserHasDevices(ctx, userID)
	if err != nil {
		return Summary{}, fmt.Errorf("checking devices: %w", err)
	}
	var credited []db.StatsCreditedBreakdownRow
	if hasDevices {
		credited, err = s.q.StatsCreditedBreakdown(ctx, db.StatsCreditedBreakdownParams{
			Tz: tz.String(), UserID: userID, FromTime: from.UTC(), ToTime: to.UTC(), Project: project,
		})
		if err != nil {
			return Summary{}, fmt.Errorf("querying credited breakdown: %w", err)
		}
	}
	sum := buildSummary(rows, first, last, tz)
	if hasDevices {
		addCredited(&sum, credited)
	}
	return sum, nil
}

// buildSummary turns breakdown rows into a sorted, zero-filled Summary.
func buildSummary(rows []db.StatsBreakdownRow, first, last Date, tz *time.Location) Summary {
	sum := Summary{
		Start: first.String(), End: last.String(), Timezone: tz.String(),
		Projects: []ProjectItem{}, Languages: []Item{}, Editors: []Item{}, Categories: []Item{},
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
			sum.Projects = append(sum.Projects, ProjectItem{Name: r.Key, Seconds: r.Seconds})
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
	sortProjects(sum.Projects)
	for _, items := range [][]Item{sum.Languages, sum.Editors, sum.Categories} {
		SortItems(items)
	}
	for d := first; !last.Before(d); d = d.AddDays(1) {
		key := d.String()
		sum.Days = append(sum.Days, Day{Date: key, Seconds: perDay[key]})
	}
	return sum
}

// addCredited fills the credited fields of sum from credited-breakdown rows
// (see StatsCreditedBreakdown). Every credited field becomes non-nil, so a
// user with a device but no credited minutes in range sees zeros.
func addCredited(sum *Summary, rows []db.StatsCreditedBreakdownRow) {
	var total, multi int64
	labels := Labels{}
	perProject := map[string]int64{}
	perDay := map[string]int64{}
	for _, r := range rows {
		switch r.Dimension {
		case "credited":
			total = r.Seconds
		case "project":
			perProject[r.Key] = r.Seconds
		case "day":
			perDay[r.Key] = r.Seconds
		case "label":
			switch r.Key {
			case "you":
				labels.You = r.Seconds
			case "agent":
				labels.Agent = r.Seconds
			case "idle":
				labels.Idle = r.Seconds
			}
		case "multi_device":
			multi = r.Seconds
		}
	}
	sum.CreditedSeconds, sum.CreditedLabels, sum.MultiDeviceMinutes = &total, &labels, &multi
	for i := range sum.Projects {
		p := &sum.Projects[i]
		v := perProject[p.Name]
		p.CreditedSeconds = &v
		delete(perProject, p.Name)
	}
	// Projects with credited time but no raw time in range.
	for name, v := range perProject {
		sum.Projects = append(sum.Projects, ProjectItem{Name: name, CreditedSeconds: &v})
	}
	sortProjects(sum.Projects)
	for i := range sum.Days {
		v := perDay[sum.Days[i].Date]
		sum.Days[i].CreditedSeconds = &v
	}
}

// sortProjects orders projects by raw seconds, then credited seconds, both
// largest first, then by name.
func sortProjects(ps []ProjectItem) {
	credited := func(p ProjectItem) int64 {
		if p.CreditedSeconds == nil {
			return 0
		}
		return *p.CreditedSeconds
	}
	slices.SortFunc(ps, func(a, b ProjectItem) int {
		return cmp.Or(cmp.Compare(b.Seconds, a.Seconds), cmp.Compare(credited(b), credited(a)), cmp.Compare(a.Name, b.Name))
	})
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
// The user's qualifying days are cached; only days whose minutes changed
// since the last call are queried again.
func (s *Service) Streak(ctx context.Context, userID uuid.UUID, tz *time.Location) (Streak, error) {
	e := s.streaks.entry(userID)
	e.refresh.Lock()
	defer e.refresh.Unlock()
	now := s.now()
	switch plan := s.streaks.plan(e, tz.String(), now); {
	case plan.full:
		raw, err := s.q.StatsQualifyingDays(ctx, db.StatsQualifyingDaysParams{
			Tz: tz.String(), UserID: userID, MinSeconds: StreakMinSeconds,
		})
		if err != nil {
			return Streak{}, fmt.Errorf("querying qualifying days: %w", err)
		}
		days, err := parseDates(raw)
		if err != nil {
			return Streak{}, err
		}
		s.streaks.storeFull(e, tz.String(), days, now)
	case plan.partial:
		// Re-query the whole local days the changed instants fall on, so each
		// day's sum is complete.
		first, last := DateOf(plan.rng.From, tz), DateOf(plan.rng.To.Add(-time.Nanosecond), tz)
		raw, err := s.q.StatsQualifyingDaysBetween(ctx, db.StatsQualifyingDaysBetweenParams{
			Tz: tz.String(), UserID: userID, MinSeconds: StreakMinSeconds,
			FromTime: first.Start(tz).UTC(), ToTime: last.AddDays(1).Start(tz).UTC(),
		})
		if err == nil {
			var days []Date
			if days, err = parseDates(raw); err == nil {
				s.streaks.storeRange(e, first, last, days)
			}
		}
		if err != nil {
			s.streaks.markDirty(e, plan.rng)
			return Streak{}, fmt.Errorf("querying qualifying days: %w", err)
		}
	}
	return computeStreak(s.streaks.snapshot(e), DateOf(now, tz)), nil
}

func parseDates(raw []string) ([]Date, error) {
	days := make([]Date, 0, len(raw))
	for _, r := range raw {
		d, err := ParseDate(r)
		if err != nil {
			return nil, fmt.Errorf("reading qualifying day: %w", err)
		}
		days = append(days, d)
	}
	return days, nil
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
