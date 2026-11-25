package programapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/stats"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// ErrUserNotFound is returned when an identifier names no user.
var ErrUserNotFound = errors.New("user not found")

// Querier is the subset of the generated queries the service reads.
type Querier interface {
	ProgramUserByPublicID(ctx context.Context, publicID int64) (db.ProgramUserByPublicIDRow, error)
	ProgramUserByUsername(ctx context.Context, username string) (db.ProgramUserByUsernameRow, error)
	ProgramUserByID(ctx context.Context, id uuid.UUID) (db.ProgramUserByIDRow, error)
	ProgramHackatimeLink(ctx context.Context, userID uuid.UUID) (db.ProgramHackatimeLinkRow, error)
	ProgramProjectSettings(ctx context.Context, userID uuid.UUID) ([]db.ProgramProjectSettingsRow, error)
	ProgramRawTotals(ctx context.Context, arg db.ProgramRawTotalsParams) ([]db.ProgramRawTotalsRow, error)
	ProgramRawTimeline(ctx context.Context, arg db.ProgramRawTimelineParams) ([]db.ProgramRawTimelineRow, error)
	ProgramRawUniqueCoding(ctx context.Context, arg db.ProgramRawUniqueCodingParams) (int64, error)
	ProgramCreditedTotals(ctx context.Context, arg db.ProgramCreditedTotalsParams) ([]db.ProgramCreditedTotalsRow, error)
	ProgramHeartbeatCounts(ctx context.Context, arg db.ProgramHeartbeatCountsParams) ([]db.ProgramHeartbeatCountsRow, error)
	ProgramHeartbeatRuns(ctx context.Context, arg db.ProgramHeartbeatRunsParams) ([]db.ProgramHeartbeatRunsRow, error)
	ProgramHeartbeats(ctx context.Context, arg db.ProgramHeartbeatsParams) ([]db.ProgramHeartbeatsRow, error)
}

// Streaks gives a user's current streak; *stats.Service implements it.
type Streaks interface {
	Location(ctx context.Context, userID uuid.UUID) (*time.Location, error)
	Streak(ctx context.Context, userID uuid.UUID, tz *time.Location) (stats.Streak, error)
}

// Service reads what the program API shows about one user at a time. Every
// method takes the user id the handler has already authorised.
type Service struct {
	q       Querier
	streaks Streaks
	now     func() time.Time
}

// NewService returns a Service. now may be nil (time.Now).
func NewService(q Querier, streaks Streaks, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{q: q, streaks: streaks, now: now}
}

// Now returns the service clock's current time.
func (s *Service) Now() time.Time { return s.now() }

// User is a Logbook user as the program API names them.
type User struct {
	ID            uuid.UUID
	PublicID      int64
	Username      string
	DisplayName   string
	Email         string
	ProfilePublic bool
}

// Name is Hackatime's display_name: the display name, else the username.
func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// UserByID loads a user by internal id.
func (s *Service) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	r, err := s.q.ProgramUserByID(ctx, id)
	if store.IsNotFound(err) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("loading user: %w", err)
	}
	return User(r), nil
}

// Lookup resolves a path identifier other than "my" (see classifyIdent):
// digits name a public_id and, failing that, a numeric username; anything
// else a username, case-insensitively.
func (s *Service) Lookup(ctx context.Context, ident string) (User, error) {
	switch classifyIdent(ident, false) {
	case identNumeric:
		if n, err := strconv.ParseInt(ident, 10, 64); err == nil {
			r, err := s.q.ProgramUserByPublicID(ctx, n)
			switch {
			case err == nil:
				return User(r), nil
			case !store.IsNotFound(err):
				return User{}, fmt.Errorf("looking up public id: %w", err)
			}
		}
		if !usernameOK(ident) {
			return User{}, ErrUserNotFound
		}
		return s.byUsername(ctx, ident)
	case identUsername:
		return s.byUsername(ctx, ident)
	default:
		return User{}, ErrUserNotFound
	}
}

func (s *Service) byUsername(ctx context.Context, name string) (User, error) {
	r, err := s.q.ProgramUserByUsername(ctx, name)
	if store.IsNotFound(err) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("looking up username: %w", err)
	}
	return User(r), nil
}

// Link is what the program API shows of a user's Hackatime link.
type Link struct {
	Trust Trust
	// HackatimeID and GitHubUsername are set only when the user linked
	// Hackatime (GitHubUsername only when Hackatime reported one).
	HackatimeID    *int64
	GitHubUsername *string
}

// Link returns the user's masked trust level and Hackatime identifiers.
func (s *Service) Link(ctx context.Context, userID uuid.UUID) (Link, error) {
	r, err := s.q.ProgramHackatimeLink(ctx, userID)
	if store.IsNotFound(err) {
		return Link{Trust: MaskTrust("", false)}, nil
	}
	if err != nil {
		return Link{}, fmt.Errorf("reading hackatime link: %w", err)
	}
	l := Link{Trust: MaskTrust(r.TrustLevel, true), HackatimeID: &r.HackatimeUserID}
	if r.HackatimeUsername != "" {
		l.GitHubUsername = &r.HackatimeUsername
	}
	return l, nil
}

// Settings are the user's per-project settings that change what programs see.
type Settings struct {
	Hidden   []string // never shown to programs
	Archived map[string]bool
	RepoURL  map[string]string
}

// Settings loads the user's project settings.
func (s *Service) Settings(ctx context.Context, userID uuid.UUID) (Settings, error) {
	rows, err := s.q.ProgramProjectSettings(ctx, userID)
	if err != nil {
		return Settings{}, fmt.Errorf("reading project settings: %w", err)
	}
	st := Settings{Hidden: []string{}, Archived: map[string]bool{}, RepoURL: map[string]string{}}
	for _, r := range rows {
		if r.Hidden {
			st.Hidden = append(st.Hidden, r.Name)
		}
		if r.Archived {
			st.Archived[r.Name] = true
		}
		if r.RepoUrl != "" {
			st.RepoURL[r.Name] = r.RepoUrl
		}
	}
	return st, nil
}

// Range is the half-open interval [From, To).
type Range struct {
	From, To time.Time
}

// RawQuery narrows raw (heartbeat) time. Nil slices mean no filter.
type RawQuery struct {
	Projects          []string
	Categories        []string
	ExcludeCategories []string // lower case
	ExcludeProjects   []string
}

// RawRow is raw seconds for one (project, language) in a range. Seconds is
// whole for rollup rows and fractional for timeline rows (RawTimeline);
// breakdowns sum rows first and round each entry once (roundSeconds).
type RawRow struct {
	Project, Language string
	Seconds           float64
	First, Last       time.Time // first and last minute
}

// Raw returns raw seconds per (project, language) in rng.
func (s *Service) Raw(ctx context.Context, userID uuid.UUID, rng Range, q RawQuery) ([]RawRow, error) {
	rows, err := s.q.ProgramRawTotals(ctx, db.ProgramRawTotalsParams{
		UserID: userID, FromTime: rng.From, ToTime: rng.To,
		Projects: q.Projects, Categories: q.Categories,
		ExcludeCategories: orEmpty(q.ExcludeCategories), ExcludeProjects: orEmpty(q.ExcludeProjects),
	})
	if err != nil {
		return nil, fmt.Errorf("querying raw totals: %w", err)
	}
	out := make([]RawRow, len(rows))
	for i, r := range rows {
		out[i] = RawRow{Project: r.Project, Language: r.Language, Seconds: float64(r.Seconds), First: r.FirstMinute, Last: r.LastMinute}
	}
	return out, nil
}

// Filtered reports whether q narrows the user's heartbeats by project or
// category (hidden projects aside).
func (q RawQuery) Filtered() bool {
	return q.Projects != nil || q.Categories != nil || len(q.ExcludeCategories) > 0
}

// RawTimeline returns raw seconds per (project, language) in rng, and their
// total, as Hackatime computes them for a filtered request: from the
// filtered heartbeats, each credited its gap to the previous one, capped at
// two minutes. boundary is Hackatime's boundary-aware total (test_param):
// the last filtered heartbeat before rng starts the first gap, and
// heartbeats at rng.To count. Project names are as reported ("Unknown" for
// none); First and Last are not set. Row seconds are fractional; the total
// is rounded once, so it may differ by a second from the rounded rows.
func (s *Service) RawTimeline(ctx context.Context, userID uuid.UUID, rng Range, q RawQuery, boundary bool) ([]RawRow, int64, error) {
	rows, err := s.q.ProgramRawTimeline(ctx, db.ProgramRawTimelineParams{
		UserID: userID, FromTime: rng.From, ToTime: rng.To, Boundary: boundary,
		Projects: q.Projects, Categories: q.Categories,
		ExcludeCategories: orEmpty(q.ExcludeCategories), ExcludeProjects: orEmpty(q.ExcludeProjects),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("querying raw timeline: %w", err)
	}
	out := make([]RawRow, 0, len(rows))
	var total int64
	for _, r := range rows {
		if r.IsTotal {
			total = int64(r.Seconds) // rounded in SQL already
			continue
		}
		out = append(out, RawRow{Project: r.Project, Language: r.Language, Seconds: r.Seconds})
	}
	return out, total, nil
}

// RawUniqueCoding is Hackatime's unique_heartbeat_seconds for projects in
// rng: the gaps of at most two minutes between consecutive "coding"
// heartbeats of those projects, in whole seconds.
func (s *Service) RawUniqueCoding(ctx context.Context, userID uuid.UUID, rng Range, projects, exclude []string) (int64, error) {
	n, err := s.q.ProgramRawUniqueCoding(ctx, db.ProgramRawUniqueCodingParams{
		UserID: userID, FromTime: rng.From, ToTime: rng.To,
		Projects: orEmpty(projects), ExcludeProjects: orEmpty(exclude),
	})
	if err != nil {
		return 0, fmt.Errorf("querying unique coding time: %w", err)
	}
	return n, nil
}

// CreditedRow is counted seconds for one project in a range.
type CreditedRow struct {
	Project     string
	Seconds     int64
	First, Last time.Time // first and last credited minute
}

// Credited returns counted (capped credited) seconds per project in rng.
// projects nil means every project.
func (s *Service) Credited(ctx context.Context, userID uuid.UUID, rng Range, projects, exclude []string) ([]CreditedRow, error) {
	rows, err := s.q.ProgramCreditedTotals(ctx, db.ProgramCreditedTotalsParams{
		UserID: userID, FromTime: rng.From, ToTime: rng.To,
		Projects: projects, ExcludeProjects: orEmpty(exclude),
	})
	if err != nil {
		return nil, fmt.Errorf("querying credited totals: %w", err)
	}
	out := make([]CreditedRow, len(rows))
	for i, r := range rows {
		out[i] = CreditedRow{Project: r.Project, Seconds: r.Seconds, First: r.FirstMinute, Last: r.LastMinute}
	}
	return out, nil
}

// HeartbeatCount is the heartbeats of one project in a range.
type HeartbeatCount struct {
	Count       int64
	First, Last time.Time
}

// HeartbeatCounts counts heartbeats per non-empty project in [from, to].
func (s *Service) HeartbeatCounts(ctx context.Context, userID uuid.UUID, from, to time.Time, projects, exclude []string) (map[string]HeartbeatCount, error) {
	rows, err := s.q.ProgramHeartbeatCounts(ctx, db.ProgramHeartbeatCountsParams{
		UserID: userID, FromTime: from, ToTime: to, Projects: projects, ExcludeProjects: orEmpty(exclude),
	})
	if err != nil {
		return nil, fmt.Errorf("counting heartbeats: %w", err)
	}
	out := make(map[string]HeartbeatCount, len(rows))
	for _, r := range rows {
		out[r.Project] = HeartbeatCount{Count: r.Heartbeats, First: r.FirstTime, Last: r.LastTime}
	}
	return out, nil
}

// spanTimeout is Hackatime's heartbeat_timeout_duration (2 minutes).
const spanTimeout = 120.0

// Span is one of Hackatime's heartbeat spans.
type Span struct {
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
	Duration  float64 `json:"duration"`
}

// Spans splits the user's heartbeats in [from, to] into spans exactly as
// Hackatime's to_span does: a span ends where the gap to the next heartbeat
// exceeds two minutes, and every span but the last gets the two-minute tail.
func (s *Service) Spans(ctx context.Context, userID uuid.UUID, from, to time.Time, projects, exclude []string) ([]Span, error) {
	runs, err := s.q.ProgramHeartbeatRuns(ctx, db.ProgramHeartbeatRunsParams{
		UserID: userID, FromTime: from, ToTime: to, Projects: projects,
		ExcludeProjects: orEmpty(exclude), GapSeconds: spanTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("querying heartbeat runs: %w", err)
	}
	return spansFromRuns(runs), nil
}

// spansFromRuns turns runs of heartbeats (first and last time of each,
// oldest first) into spans.
func spansFromRuns(runs []db.ProgramHeartbeatRunsRow) []Span {
	spans := make([]Span, 0, len(runs))
	for i, r := range runs {
		base := roundHalfAway(r.LastTime - r.FirstTime)
		if i < len(runs)-1 {
			// The next run starts more than spanTimeout later, so the tail
			// is capped at spanTimeout.
			spans = append(spans, Span{StartTime: r.FirstTime, EndTime: r.LastTime + spanTimeout, Duration: base + spanTimeout})
			continue
		}
		if base > 0 {
			spans = append(spans, Span{StartTime: r.FirstTime, EndTime: r.LastTime, Duration: base})
		}
	}
	return spans
}

// roundHalfAway is Ruby's Float#round: halves away from zero.
func roundHalfAway(f float64) float64 {
	if f < 0 {
		return -roundHalfAway(-f)
	}
	return float64(int64(f + 0.5))
}

// Heartbeats returns up to limit of the user's heartbeats in [from, to],
// leaving out the projects in exclude.
func (s *Service) Heartbeats(ctx context.Context, userID uuid.UUID, from, to time.Time, exclude []string, limit int32) ([]db.ProgramHeartbeatsRow, error) {
	rows, err := s.q.ProgramHeartbeats(ctx, db.ProgramHeartbeatsParams{UserID: userID, FromTime: from, ToTime: to, ExcludeProjects: orEmpty(exclude), MaxRows: limit})
	if err != nil {
		return nil, fmt.Errorf("reading heartbeats: %w", err)
	}
	return rows, nil
}

// Streak is the user's current streak in days (stats.Service's rule: local
// days with at least 15 minutes, counting back from today).
func (s *Service) Streak(ctx context.Context, userID uuid.UUID) (int, error) {
	tz, err := s.streaks.Location(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("loading time zone: %w", err)
	}
	st, err := s.streaks.Streak(ctx, userID, tz)
	if err != nil {
		return 0, fmt.Errorf("computing streak: %w", err)
	}
	return st.Current, nil
}

// orEmpty turns nil into an empty slice, for "NOT x = ANY(...)" arguments
// that must not be NULL.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
