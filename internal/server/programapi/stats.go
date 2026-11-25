package programapi

import (
	"cmp"
	"context"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Categories Hackatime leaves out of test_param totals, plus "ai coding"
// with no_ai_coding (stats_controller.rb).
var (
	testExcludedCategories = []string{"browsing", "meeting", "communicating"}
	aiCodingCategory       = "ai coding"
)

// noteRawLanguages is the logbook.notes entry for the languages breakdown.
const noteRawLanguages = "languages are time at the keyboard (raw heartbeat time): counted time is kept per project only"

// StatsRequest is a parsed GET /api/v1/users/{id}/stats.
type StatsRequest struct {
	Range Range
	// Languages and Projects say which breakdowns were asked for
	// (features; Hackatime builds only these two). ProjectsFeature is
	// Hackatime's params[:features].include?("projects"), a substring test.
	Languages, Projects bool
	ProjectsFeature     bool
	Limit               int
	FilterProjects      []string // filter_by_project; nil means all, empty means none
	FilterCategories    []string // filter_by_category; nil means all, empty means none
	TotalOnly           bool     // total_seconds=true
	Test                bool     // test_param=true
	NoAICoding          bool     // no_ai_coding=true
	Raw                 bool     // raw=true: report time at the keyboard
}

// ParamError is a parameter the client got wrong. Message is Hackatime's.
type ParamError struct{ Message string }

func (e *ParamError) Error() string { return e.Message }

// ParseStatsRequest reads the stats parameters exactly as Hackatime's
// user_stats does. now is the request time.
func ParseStatsRequest(q url.Values, now time.Time) (StatsRequest, error) {
	rng, err := statsRange(q, now)
	if err != nil {
		return StatsRequest{}, err
	}
	req := StatsRequest{
		Range:            rng,
		Limit:            max(rubyToI(q.Get("limit")), 0),
		FilterProjects:   listParam(q, "filter_by_project"),
		FilterCategories: listParam(q, "filter_by_category"),
		TotalOnly:        isTrue(q, "total_seconds"),
		Test:             isTrue(q, "test_param"),
		NoAICoding:       isTrue(q, "no_ai_coding"),
		Raw:              isTrue(q, "raw"),
	}
	if !validList(req.FilterProjects) || !validList(req.FilterCategories) {
		return StatsRequest{}, &ParamError{Message: "Invalid filter"}
	}
	features, has := q["features"]
	if !has {
		req.Languages = true // Hackatime's default: %i[languages]
	} else {
		raw := features[0]
		for _, f := range strings.Split(raw, ",") {
			switch f {
			case "languages":
				req.Languages = true
			case "projects":
				req.Projects = true
			}
		}
		req.ProjectsFeature = strings.Contains(raw, "projects")
	}
	return req, nil
}

// statsRange reads start_date and end_date (Hackatime's
// parse_datetime_param): defaults are ten years ago and the end of today
// (UTC), and an unparsable value is a 422. Bounds are whole Unix seconds, as
// Hackatime's WakatimeService truncates them.
func statsRange(q url.Values, now time.Time) (Range, error) {
	from := now.UTC().AddDate(-10, 0, 0)
	if t, present, ok := timeParam(q, "start_date"); present && !ok {
		return Range{}, &ParamError{Message: "Invalid start_date"}
	} else if present {
		from = t
	}
	to := endOfDay(now)
	if t, present, ok := timeParam(q, "end_date"); present && !ok {
		return Range{}, &ParamError{Message: "Invalid end_date"}
	} else if present {
		to = t
	}
	return Range{From: from.Truncate(time.Second), To: to.Truncate(time.Second)}, nil
}

// endOfDay is the last microsecond of t's UTC day (Rails' end_of_day).
func endOfDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 23, 59, 59, 999_999_000, time.UTC)
}

// Totals is the total_seconds=true body.
type Totals struct {
	TotalSeconds    int64 `json:"total_seconds"`
	CreditedSeconds int64 `json:"credited_seconds"`
	RawSeconds      int64 `json:"raw_seconds"`
}

// Chunk is one row of a languages or projects breakdown
// (WakatimeService#generate_summary_chunk).
type Chunk struct {
	Name         string  `json:"name"`
	TotalSeconds int64   `json:"total_seconds"`
	Text         string  `json:"text"`
	Hours        int64   `json:"hours"`
	Minutes      int64   `json:"minutes"`
	Percent      float64 `json:"percent"`
	Digital      string  `json:"digital"`
	// Color is set on languages only. Logbook doesn't ship GitHub's
	// linguist colour table, so it is always Hackatime's default.
	Color string `json:"color,omitempty"`
	// Set on projects only.
	CreditedSeconds *int64 `json:"credited_seconds,omitempty"`
	RawSeconds      *int64 `json:"raw_seconds,omitempty"`
}

// defaultLanguageColor is Hackatime's LanguageUtils::DEFAULT_COLOR.
const defaultLanguageColor = "#888888"

// Summary is the stats body's "data" object (WakatimeService#build_summary),
// in Hackatime's key order, plus credited_seconds and raw_seconds.
type Summary struct {
	Username                  string   `json:"username"`
	UserID                    string   `json:"user_id"`
	IsCodingActivityVisible   bool     `json:"is_coding_activity_visible"`
	IsOtherUsageVisible       bool     `json:"is_other_usage_visible"`
	Status                    string   `json:"status"`
	Start                     string   `json:"start"`
	End                       string   `json:"end"`
	Range                     string   `json:"range"`
	HumanReadableRange        string   `json:"human_readable_range"`
	TotalSeconds              int64    `json:"total_seconds"`
	DailyAverage              int64    `json:"daily_average"`
	HumanReadableTotal        string   `json:"human_readable_total"`
	HumanReadableDailyAverage string   `json:"human_readable_daily_average"`
	Languages                 *[]Chunk `json:"languages,omitempty"`
	Projects                  *[]Chunk `json:"projects,omitempty"`
	UniqueTotalSeconds        *int64   `json:"unique_total_seconds,omitempty"`
	Streak                    int      `json:"streak"`
	CreditedSeconds           int64    `json:"credited_seconds"`
	RawSeconds                int64    `json:"raw_seconds"`
}

// TrustFactor is Hackatime's {trust_level, trust_value}.
type TrustFactor struct {
	TrustLevel string `json:"trust_level"`
	TrustValue int    `json:"trust_value"`
}

// Extension is the top-level "logbook" object (docs/api/programs.md).
type Extension struct {
	Username      string   `json:"username,omitempty"`
	HackatimeID   *int64   `json:"hackatime_id,omitempty"`
	TrustVerified *bool    `json:"trust_verified,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

// StatsBody is the stats response without total_seconds=true.
type StatsBody struct {
	Data        Summary     `json:"data"`
	TrustFactor TrustFactor `json:"trust_factor"`
	Logbook     Extension   `json:"logbook"`
}

func trustFactor(t Trust) TrustFactor { return TrustFactor{TrustLevel: t.Level, TrustValue: t.Value} }

// rawQuery is which raw time a stats request counts. Hackatime applies
// filter_by_category only to the plain total_seconds=true path, and its
// category exclusions only with test_param or no_ai_coding. no_ai_coding
// compares lower(category) on the summary and test_param paths
// (WakatimeService's exclude_categories) but the exact category on the
// plain total_seconds=true path (where.not(category: "ai coding")).
func (r StatsRequest) rawQuery(hidden []string) RawQuery {
	q := RawQuery{Projects: r.FilterProjects, ExcludeProjects: hidden}
	if r.Test {
		q.ExcludeCategories = slices.Clone(testExcludedCategories)
	} else if r.TotalOnly {
		q.Categories = r.FilterCategories
	}
	if r.NoAICoding {
		if r.TotalOnly && !r.Test {
			q.ExcludeCategoriesExact = []string{aiCodingCategory}
		} else {
			q.ExcludeCategories = append(q.ExcludeCategories, aiCodingCategory)
		}
	}
	return q
}

// statsRaw reads the raw time of a stats request and its total. Without a
// project or category filter it is the rollup (time at the keyboard, less
// hidden projects). Hackatime filters heartbeats before it takes the gaps
// between them, so a filtered request is computed the same way from the
// filtered heartbeats (RawTimeline), boundary-aware with test_param as
// Hackatime's total is; its rows share that one timeline.
func (s *Service) statsRaw(ctx context.Context, u User, req StatsRequest, hidden []string) ([]RawRow, int64, error) {
	q := req.rawQuery(hidden)
	if !q.Filtered() {
		raw, err := s.Raw(ctx, u.ID, req.Range, q)
		return raw, sumRaw(raw), err
	}
	return s.RawTimeline(ctx, u.ID, req.Range, q, req.Test)
}

// StatsTotals answers a total_seconds=true request.
func (s *Service) StatsTotals(ctx context.Context, u User, req StatsRequest) (Totals, error) {
	st, err := s.Settings(ctx, u.ID)
	if err != nil {
		return Totals{}, err
	}
	_, rawTotal, err := s.statsRaw(ctx, u, req, st.Hidden)
	if err != nil {
		return Totals{}, err
	}
	credited, err := s.Credited(ctx, u.ID, req.Range, req.FilterProjects, st.Hidden)
	if err != nil {
		return Totals{}, err
	}
	t := Totals{RawSeconds: rawTotal, CreditedSeconds: sumCredited(credited)}
	t.TotalSeconds = pick(req.Raw, t.RawSeconds, t.CreditedSeconds)
	return t, nil
}

// Stats answers a stats request without total_seconds=true.
func (s *Service) Stats(ctx context.Context, u User, req StatsRequest) (StatsBody, error) {
	st, err := s.Settings(ctx, u.ID)
	if err != nil {
		return StatsBody{}, err
	}
	raw, rawTotal, err := s.statsRaw(ctx, u, req, st.Hidden)
	if err != nil {
		return StatsBody{}, err
	}
	credited, err := s.Credited(ctx, u.ID, req.Range, req.FilterProjects, st.Hidden)
	if err != nil {
		return StatsBody{}, err
	}
	link, err := s.Link(ctx, u.ID)
	if err != nil {
		return StatsBody{}, err
	}
	streak, err := s.Streak(ctx, u.ID)
	if err != nil {
		return StatsBody{}, err
	}
	creditedTotal := sumCredited(credited)
	total := pick(req.Raw, rawTotal, creditedTotal)
	sum := Summary{
		Username:                u.Name(),
		UserID:                  strconv.FormatInt(u.PublicID, 10),
		IsCodingActivityVisible: true,
		IsOtherUsageVisible:     true,
		Status:                  "ok",
		Start:                   isoSeconds(req.Range.From),
		End:                     isoSeconds(req.Range.To),
		Range:                   "all_time",
		HumanReadableRange:      "All Time",
		TotalSeconds:            total,
		HumanReadableTotal:      shortTimeDetailed(total),
		Streak:                  streak,
		CreditedSeconds:         creditedTotal,
		RawSeconds:              rawTotal,
	}
	if days := (req.Range.To.Unix() - req.Range.From.Unix()) / 86400; days != 0 {
		sum.DailyAverage = total / days
	}
	sum.HumanReadableDailyAverage = shortTimeDetailed(sum.DailyAverage)
	var ext Extension
	verified := link.Trust.Verified
	ext.TrustVerified = &verified
	if req.Languages {
		langs := languageChunks(raw, rawTotal, req.Limit)
		sum.Languages = &langs
		ext.Notes = append(ext.Notes, noteRawLanguages)
	}
	if req.Projects {
		projects := projectChunks(raw, credited, total, req.Raw, req.Limit)
		sum.Projects = &projects
	}
	if req.ProjectsFeature && req.FilterProjects != nil { // Hackatime: filter_by_project.present?
		// Counted: the filtered projects' counted time, capped at 60 s per
		// minute across projects already, so never double counted. Raw:
		// Hackatime's unique_heartbeat_seconds.
		unique := creditedTotal
		if req.Raw {
			if unique, err = s.RawUniqueCoding(ctx, u.ID, req.Range, req.FilterProjects, st.Hidden); err != nil {
				return StatsBody{}, err
			}
		}
		sum.UniqueTotalSeconds = &unique
	}
	return StatsBody{Data: sum, TrustFactor: trustFactor(link.Trust), Logbook: ext}, nil
}

// languageChunks builds the languages breakdown from raw rows: always time
// at the keyboard, with percentages of the raw total.
func languageChunks(raw []RawRow, rawTotal int64, limit int) []Chunk {
	by := map[string]float64{}
	for _, r := range raw {
		by[displayName(r.Language)] += r.Seconds
	}
	out := make([]Chunk, 0, len(by))
	for name, secs := range by {
		c := chunk(name, roundSeconds(secs), rawTotal)
		c.Color = defaultLanguageColor
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Chunk) int {
		return cmp.Or(cmp.Compare(b.TotalSeconds, a.TotalSeconds), cmp.Compare(a.Name, b.Name))
	})
	return limitChunks(out, limit)
}

// projectChunks builds the projects breakdown: every project with raw or
// counted time in range, total_seconds counted (or raw with raw=true), and
// percentages of the reported total.
func projectChunks(raw []RawRow, credited []CreditedRow, total int64, useRaw bool, limit int) []Chunk {
	rawExact, creditedBy := map[string]float64{}, map[string]int64{}
	for _, r := range raw {
		rawExact[displayName(r.Project)] += r.Seconds
	}
	rawBy := make(map[string]int64, len(rawExact))
	for n, secs := range rawExact {
		rawBy[n] = roundSeconds(secs)
	}
	for _, c := range credited {
		creditedBy[displayName(c.Project)] += c.Seconds
	}
	names := map[string]bool{}
	for n := range rawBy {
		names[n] = true
	}
	for n := range creditedBy {
		names[n] = true
	}
	out := make([]Chunk, 0, len(names))
	for n := range names {
		r, c := rawBy[n], creditedBy[n]
		ch := chunk(n, pick(useRaw, r, c), total)
		ch.CreditedSeconds, ch.RawSeconds = &c, &r
		out = append(out, ch)
	}
	slices.SortFunc(out, func(a, b Chunk) int {
		return cmp.Or(cmp.Compare(b.TotalSeconds, a.TotalSeconds), cmp.Compare(*b.RawSeconds, *a.RawSeconds), cmp.Compare(a.Name, b.Name))
	})
	return limitChunks(out, limit)
}

func chunk(name string, secs, total int64) Chunk {
	return Chunk{
		Name: name, TotalSeconds: secs, Text: shortTimeSimple(secs),
		Hours: secs / 3600, Minutes: secs % 3600 / 60,
		Percent: percent(secs, total), Digital: digitalTime(secs),
	}
}

// limitChunks keeps the first limit rows; 0 means all (Hackatime's limit
// param, where 0 is "no limit").
func limitChunks(cs []Chunk, limit int) []Chunk {
	if limit > 0 && len(cs) > limit {
		return cs[:limit]
	}
	return cs
}

// displayName is Hackatime's key.presence || "Other".
func displayName(s string) string {
	if s == "" {
		return "Other"
	}
	return s
}

// sumRaw totals raw rows, rounding once.
func sumRaw(rows []RawRow) int64 {
	var n float64
	for _, r := range rows {
		n += r.Seconds
	}
	return roundSeconds(n)
}

// roundSeconds rounds a sum of fractional seconds to whole seconds as
// Hackatime does, by casting to integer in Postgres: half to even.
func roundSeconds(f float64) int64 { return int64(math.RoundToEven(f)) }

func sumCredited(rows []CreditedRow) int64 {
	var n int64
	for _, r := range rows {
		n += r.Seconds
	}
	return n
}

// pick returns raw when useRaw, else counted.
func pick(useRaw bool, raw, counted int64) int64 {
	if useRaw {
		return raw
	}
	return counted
}
