package programapi

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"time"

	"github.com/google/uuid"
)

// ProjectQuery is a parsed project-details request (Hackatime's
// ProjectStatsQuery): which projects, over which range.
type ProjectQuery struct {
	Range           Range
	Names           []string // nil means every project
	IncludeArchived bool
	Raw             bool // raw=true: total_seconds is time at the keyboard
}

// projectStatsRange reads the stats range of ProjectStatsQuery: start or
// start_date, end or end_date, with the given defaults; unparsable values
// fall back to the default (never an error).
func projectStatsRange(q url.Values, defFrom, defTo time.Time) Range {
	return Range{
		From: firstTime(q, defFrom, "start", "start_date"),
		To:   firstTime(q, defTo, "end", "end_date"),
	}
}

// projectDiscoveryRange reads ProjectStatsQuery's discovery range (for
// users/{id}/projects): since/start/start_date, until/until_date/end/end_date,
// defaulting to the start of the UTC day 30 days ago until now.
func projectDiscoveryRange(q url.Values, now time.Time) Range {
	y, m, d := now.UTC().AddDate(0, 0, -30).Date()
	return Range{
		From: firstTime(q, time.Date(y, m, d, 0, 0, 0, 0, time.UTC), "since", "start", "start_date"),
		To:   firstTime(q, now, "until", "until_date", "end", "end_date"),
	}
}

// ProjectDetail is one row of Hackatime's project_details
// (ProjectStatsQuery#build_project_row), in its key order, plus
// credited_seconds and raw_seconds.
type ProjectDetail struct {
	Name                string   `json:"name"`
	TotalSeconds        int64    `json:"total_seconds"`
	Languages           []string `json:"languages"`
	RepoURL             *string  `json:"repo_url"`
	TotalHeartbeats     int64    `json:"total_heartbeats"`
	FirstHeartbeat      *string  `json:"first_heartbeat"`
	LastHeartbeat       *string  `json:"last_heartbeat"`
	MostRecentHeartbeat *string  `json:"most_recent_heartbeat"`
	Archived            bool     `json:"archived"`
	CreditedSeconds     int64    `json:"credited_seconds"`
	RawSeconds          int64    `json:"raw_seconds"`
}

// ProjectDetails returns the user's projects with time in q.Range, largest
// total first. A project has time when it has raw minutes or counted minutes
// there; projects with an empty name, hidden projects and (unless
// q.IncludeArchived) archived projects are left out.
//
// first_heartbeat and last_heartbeat come from heartbeats in the range. For a
// project the agent credited without any editor heartbeat they are the start
// of its first and the end of its last credited minute.
func (s *Service) ProjectDetails(ctx context.Context, userID uuid.UUID, q ProjectQuery) ([]ProjectDetail, error) {
	st, err := s.Settings(ctx, userID)
	if err != nil {
		return nil, err
	}
	raw, err := s.Raw(ctx, userID, q.Range, RawQuery{Projects: q.Names, ExcludeProjects: st.Hidden})
	if err != nil {
		return nil, err
	}
	credited, err := s.Credited(ctx, userID, q.Range, q.Names, st.Hidden)
	if err != nil {
		return nil, err
	}
	counts, err := s.HeartbeatCounts(ctx, userID, q.Range.From, q.Range.To, q.Names, st.Hidden)
	if err != nil {
		return nil, err
	}
	return buildProjectDetails(raw, credited, counts, st, q), nil
}

// projectAcc gathers one project's figures.
type projectAcc struct {
	raw, credited int64
	languages     map[string]bool
	first, last   time.Time // activity bounds from minutes
}

func buildProjectDetails(raw []RawRow, credited []CreditedRow, counts map[string]HeartbeatCount, st Settings, q ProjectQuery) []ProjectDetail {
	accs := map[string]*projectAcc{}
	get := func(name string) *projectAcc {
		a, ok := accs[name]
		if !ok {
			a = &projectAcc{languages: map[string]bool{}}
			accs[name] = a
		}
		return a
	}
	widen := func(a *projectAcc, first, last time.Time) {
		if a.first.IsZero() || first.Before(a.first) {
			a.first = first
		}
		if last.After(a.last) {
			a.last = last
		}
	}
	for _, r := range raw {
		if r.Project == "" {
			continue
		}
		a := get(r.Project)
		a.raw += r.Seconds
		if r.Language != "" {
			a.languages[r.Language] = true
		}
		widen(a, r.First, r.Last.Add(time.Minute))
	}
	for _, c := range credited {
		if c.Project == "" {
			continue
		}
		a := get(c.Project)
		a.credited += c.Seconds
		widen(a, c.First, c.Last.Add(time.Minute))
	}
	out := make([]ProjectDetail, 0, len(accs))
	for name, a := range accs {
		if st.Archived[name] && !q.IncludeArchived {
			continue
		}
		d := ProjectDetail{
			Name: name, TotalSeconds: pick(q.Raw, a.raw, a.credited),
			Languages: sortedKeys(a.languages), Archived: st.Archived[name],
			CreditedSeconds: a.credited, RawSeconds: a.raw,
		}
		if u, ok := st.RepoURL[name]; ok {
			d.RepoURL = &u
		}
		first, last := a.first, a.last
		if c, ok := counts[name]; ok {
			d.TotalHeartbeats = c.Count
			first, last = c.First, c.Last
		}
		f, l := isoSeconds(first), isoSeconds(last)
		d.FirstHeartbeat, d.LastHeartbeat, d.MostRecentHeartbeat = &f, &l, &l
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b ProjectDetail) int {
		return cmp.Or(cmp.Compare(b.TotalSeconds, a.TotalSeconds), cmp.Compare(b.RawSeconds, a.RawSeconds), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// ProjectNames lists the names of the user's projects with raw or counted
// time in rng, archived ones included, hidden ones not, sorted by name
// (Hackatime's project_names).
func (s *Service) ProjectNames(ctx context.Context, userID uuid.UUID, rng Range) ([]string, error) {
	st, err := s.Settings(ctx, userID)
	if err != nil {
		return nil, err
	}
	raw, err := s.Raw(ctx, userID, rng, RawQuery{ExcludeProjects: st.Hidden})
	if err != nil {
		return nil, err
	}
	credited, err := s.Credited(ctx, userID, rng, nil, st.Hidden)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, r := range raw {
		if r.Project != "" {
			names[r.Project] = true
		}
	}
	for _, c := range credited {
		if c.Project != "" {
			names[c.Project] = true
		}
	}
	return sortedKeys(names), nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
