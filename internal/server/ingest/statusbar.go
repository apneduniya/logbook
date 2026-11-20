package ingest

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// StatusBar is WakaTime's GET /users/current/statusbar/today response, the
// shape wakatime-cli's --today parses (pkg/summary in wakatime-cli).
type StatusBar struct {
	CachedAt        string        `json:"cached_at"`
	Data            StatusBarData `json:"data"`
	HasTeamFeatures bool          `json:"has_team_features"`
}

// StatusBarData is the "data" object of StatusBar.
type StatusBarData struct {
	Categories       []Counter  `json:"categories"`
	Dependencies     []Counter  `json:"dependencies"`
	Editors          []Counter  `json:"editors"`
	GrandTotal       GrandTotal `json:"grand_total"`
	Languages        []Counter  `json:"languages"`
	Machines         []Counter  `json:"machines"`
	OperatingSystems []Counter  `json:"operating_systems"`
	Projects         []Counter  `json:"projects"`
	Range            DayRange   `json:"range"`
}

// GrandTotal is the day's total, the part status bars display.
type GrandTotal struct {
	Decimal      string  `json:"decimal"`
	Digital      string  `json:"digital"`
	Hours        int     `json:"hours"`
	Minutes      int     `json:"minutes"`
	Text         string  `json:"text"`
	TotalSeconds float64 `json:"total_seconds"`
}

// Counter is the time spent on one project, language, editor or category.
type Counter struct {
	Decimal      string  `json:"decimal"`
	Digital      string  `json:"digital"`
	Hours        int     `json:"hours"`
	Minutes      int     `json:"minutes"`
	Name         string  `json:"name"`
	Percent      float64 `json:"percent"`
	Seconds      int     `json:"seconds"`
	Text         string  `json:"text"`
	TotalSeconds float64 `json:"total_seconds"`
}

// DayRange describes the local day the totals cover.
type DayRange struct {
	Date     string `json:"date"`
	End      string `json:"end"`
	Start    string `json:"start"`
	Text     string `json:"text"`
	Timezone string `json:"timezone"`
}

// StatusBarToday totals userID's 'you' time for the current day in the
// user's timezone. Machines, operating systems and dependencies aren't kept
// in rollups, so those lists are always empty.
func (s *Service) StatusBarToday(ctx context.Context, userID uuid.UUID) (StatusBar, error) {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return StatusBar{}, fmt.Errorf("loading user: %w", err)
	}
	loc := s.location(ctx, u.Timezone)
	now := s.now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	end := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)

	groups, err := s.store.SumUserMinutesByGroup(ctx, db.SumUserMinutesByGroupParams{
		UserID: userID, StartTime: start.UTC(), EndTime: end.UTC(),
	})
	if err != nil {
		return StatusBar{}, fmt.Errorf("summing today's minutes: %w", err)
	}

	var total int64
	projects, languages, editors, categories := map[string]int64{}, map[string]int64{}, map[string]int64{}, map[string]int64{}
	for _, g := range groups {
		total += g.Seconds
		projects[g.Project] += g.Seconds
		languages[displayOr(g.Language, "Other")] += g.Seconds
		editors[displayOr(g.Editor, "Unknown")] += g.Seconds
		categories[categoryName(g.Category)] += g.Seconds
	}

	h, m, _ := hms(total)
	return StatusBar{
		CachedAt: s.now().UTC().Format(time.RFC3339),
		Data: StatusBarData{
			Categories:       counters(categories, total),
			Dependencies:     []Counter{},
			Editors:          counters(editors, total),
			GrandTotal:       GrandTotal{Decimal: decimal(total), Digital: fmt.Sprintf("%d:%02d", h, m), Hours: h, Minutes: m, Text: humanText(total), TotalSeconds: float64(total)},
			Languages:        counters(languages, total),
			Machines:         []Counter{},
			OperatingSystems: []Counter{},
			Projects:         counters(projects, total),
			Range: DayRange{
				Date:     start.Format(time.DateOnly),
				Start:    start.UTC().Format(time.RFC3339),
				End:      end.Add(-time.Second).UTC().Format(time.RFC3339),
				Text:     dayText(start),
				Timezone: loc.String(),
			},
		},
	}, nil
}

// location returns the named timezone, falling back to UTC for unknown
// names. Results are cached because LoadLocation reads and parses a file.
func (s *Service) location(ctx context.Context, name string) *time.Location {
	if v, ok := s.locs.Load(name); ok {
		return v.(*time.Location) //nolint:forcetypeassert // only *time.Location is stored
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Not cached, so arbitrary invalid names can't grow the cache.
		slog.WarnContext(ctx, "unknown user timezone, using UTC", "timezone", name, "err", err)
		return time.UTC
	}
	s.locs.Store(name, loc)
	return loc
}

func counters(totals map[string]int64, grand int64) []Counter {
	out := make([]Counter, 0, len(totals))
	for name, sec := range totals {
		if sec <= 0 {
			continue
		}
		h, m, s := hms(sec)
		out = append(out, Counter{
			Decimal:      decimal(sec),
			Digital:      fmt.Sprintf("%d:%02d:%02d", h, m, s),
			Hours:        h,
			Minutes:      m,
			Name:         name,
			Percent:      math.Round(float64(sec)/float64(grand)*10000) / 100,
			Seconds:      s,
			Text:         humanText(sec),
			TotalSeconds: float64(sec),
		})
	}
	slices.SortFunc(out, func(a, b Counter) int {
		return cmp.Or(cmp.Compare(b.TotalSeconds, a.TotalSeconds), cmp.Compare(a.Name, b.Name))
	})
	return out
}

func hms(sec int64) (h, m, s int) {
	return int(sec / 3600), int(sec % 3600 / 60), int(sec % 60)
}

// decimal is hours with two decimals, from whole minutes, as WakaTime shows it.
func decimal(sec int64) string {
	h, m, _ := hms(sec)
	return fmt.Sprintf("%.2f", float64(h)+float64(m)/60)
}

// humanText formats like WakaTime: "2 hrs 17 mins", "1 hr", "49 mins", "7 secs".
func humanText(sec int64) string {
	h, m, s := hms(sec)
	if h == 0 && m == 0 {
		return plural(s, "sec")
	}
	var parts []string
	if h > 0 {
		parts = append(parts, plural(h, "hr"))
	}
	if m > 0 {
		parts = append(parts, plural(m, "min"))
	}
	return strings.Join(parts, " ")
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// dayText formats like WakaTime's range text: "Sun Jan 29th 2023".
func dayText(t time.Time) string {
	d := t.Day()
	suffix := "th"
	if d < 11 || d > 13 {
		switch d % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%s %s %d%s %d", t.Format("Mon"), t.Format("Jan"), d, suffix, t.Year())
}

// categoryName turns a stored category ("coding", "ai coding") into WakaTime's
// display name ("Coding", "AI Coding").
func categoryName(c string) string {
	words := strings.Fields(c)
	for i, w := range words {
		if w == "ai" {
			words[i] = "AI"
			continue
		}
		r, size := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[size:]
	}
	return displayOr(strings.Join(words, " "), "Coding")
}

func displayOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
