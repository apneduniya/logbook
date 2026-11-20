package profiles

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// ErrNotFound means the profile doesn't exist or isn't public.
var ErrNotFound = errors.New("profile not found")

// TopProjects is how many projects a public profile lists.
const TopProjects = 10

// recentWindow is the "last 7 days" window on public profiles.
const recentWindow = 7 * 24 * time.Hour

// Querier is the subset of the generated queries the service reads with.
type Querier interface {
	ProfileGetByID(ctx context.Context, id uuid.UUID) (db.ProfileGetByIDRow, error)
	ProfileGetByUsername(ctx context.Context, username string) (db.ProfileGetByUsernameRow, error)
	ProfileTimezoneKnown(ctx context.Context, name string) (bool, error)
	ProfilePublicTotals(ctx context.Context, arg db.ProfilePublicTotalsParams) ([]db.ProfilePublicTotalsRow, error)
	ProfilePublicCredited(ctx context.Context, arg db.ProfilePublicCreditedParams) ([]db.ProfilePublicCreditedRow, error)
	StatsUserHasDevices(ctx context.Context, userID uuid.UUID) (bool, error)
}

// Store is what the service needs: reads plus transactions for updates.
// *store.Store satisfies it.
type Store interface {
	Querier
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
}

// PrivacyListener is told when a profile stops being public, so open public
// streams can be closed. live.Hub implements it.
type PrivacyListener interface {
	ProfileMadePrivate(userID uuid.UUID)
}

// Service reads and updates profiles.
type Service struct {
	st       Store
	listener PrivacyListener
	now      func() time.Time
	cache    *aggCache
}

// NewService returns a Service. listener may be nil.
func NewService(st Store, listener PrivacyListener) *Service {
	return &Service{st: st, listener: listener, now: time.Now, cache: newAggCache(time.Now)}
}

// Me is the signed-in user's own profile settings.
type Me struct {
	Username      string    `json:"username"`
	DisplayName   string    `json:"display_name"`
	AvatarURL     string    `json:"avatar_url"`
	Timezone      string    `json:"timezone"`
	ProfilePublic bool      `json:"profile_public"`
	MemberSince   time.Time `json:"member_since"`
}

// Item is a named total, such as a language.
type Item struct {
	Name    string `json:"name"`
	Seconds int64  `json:"seconds"`
}

// Totals are seconds over all time and the last 7 days: raw (heartbeat)
// time, and the agent's credited time (ADR 0014). The credited fields are
// null when the user has never registered a device.
type Totals struct {
	AllTimeSeconds           int64  `json:"all_time_seconds"`
	Last7DaysSeconds         int64  `json:"last_7_days_seconds"`
	CreditedAllTimeSeconds   *int64 `json:"credited_all_time_seconds"`
	CreditedLast7DaysSeconds *int64 `json:"credited_last_7_days_seconds"`
}

// PublicProject is a non-hidden project as shown on a public profile.
type PublicProject struct {
	Name                     string `json:"name"`
	RepoURL                  string `json:"repo_url"`
	TotalSeconds             int64  `json:"total_seconds"`
	Last7DaysSeconds         int64  `json:"last_7_days_seconds"`
	CreditedSeconds          *int64 `json:"credited_seconds"`
	CreditedLast7DaysSeconds *int64 `json:"credited_last_7_days_seconds"`
}

// Public is a public profile. Hidden projects contribute nothing to it.
type Public struct {
	Username    string          `json:"username"`
	DisplayName string          `json:"display_name"`
	AvatarURL   string          `json:"avatar_url"`
	MemberSince time.Time       `json:"member_since"`
	Totals      Totals          `json:"totals"`
	Projects    []PublicProject `json:"projects"`
	Languages   []Item          `json:"languages"`
}

// Patch holds the settings to change; nil fields are left as they are.
type Patch struct {
	DisplayName   *string
	Timezone      *string
	ProfilePublic *bool
}

// PublicUserID returns the id of a user whose profile is public. ok is false
// for unknown usernames and private profiles alike.
func (s *Service) PublicUserID(ctx context.Context, username string) (id uuid.UUID, ok bool, err error) {
	if !ValidUsername(username) {
		return uuid.Nil, false, nil
	}
	u, err := s.st.ProfileGetByUsername(ctx, username)
	if store.IsNotFound(err) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("reading user: %w", err)
	}
	if !u.ProfilePublic {
		return uuid.Nil, false, nil
	}
	return u.ID, true, nil
}

// Public returns username's public profile, or ErrNotFound when it is
// missing or private.
func (s *Service) Public(ctx context.Context, username string) (Public, error) {
	if !ValidUsername(username) {
		return Public{}, ErrNotFound
	}
	u, err := s.st.ProfileGetByUsername(ctx, username)
	if store.IsNotFound(err) {
		return Public{}, ErrNotFound
	}
	if err != nil {
		return Public{}, fmt.Errorf("reading user: %w", err)
	}
	// Privacy is read fresh on every request; only the aggregates are cached.
	if !u.ProfilePublic {
		return Public{}, ErrNotFound
	}
	agg, err := s.aggregates(ctx, u.ID, time.Time{})
	if err != nil {
		return Public{}, err
	}
	p := agg.public
	p.Username, p.DisplayName, p.AvatarURL, p.MemberSince = u.Username, u.DisplayName, u.AvatarUrl, u.CreatedAt
	return p, nil
}

// PublicVersion fingerprints the time a public profile shows, from data read
// no earlier than freshAfter (zero accepts the cached value). It changes
// only when credited time in non-hidden projects, or their repo links,
// change; hidden projects, agent and idle time never move it. ok is false
// when the user is missing or the profile is private.
func (s *Service) PublicVersion(ctx context.Context, userID uuid.UUID, freshAfter time.Time) (version string, ok bool, err error) {
	u, err := s.st.ProfileGetByID(ctx, userID)
	if store.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading user: %w", err)
	}
	if !u.ProfilePublic {
		return "", false, nil
	}
	agg, err := s.aggregates(ctx, userID, freshAfter)
	if err != nil {
		return "", false, err
	}
	return agg.version, true, nil
}

// ProjectSettingsChanged drops the user's cached aggregates, so hiding a
// project or changing its repo link shows on the public profile at once.
// projects.Service calls it after each change.
func (s *Service) ProjectSettingsChanged(userID uuid.UUID) {
	s.cache.invalidate(userID)
}

func (s *Service) aggregates(ctx context.Context, userID uuid.UUID, freshAfter time.Time) (aggregates, error) {
	return s.cache.get(ctx, userID, freshAfter, func(ctx context.Context) (aggregates, error) {
		since := s.now().Add(-recentWindow).UTC()
		rows, err := s.st.ProfilePublicTotals(ctx, db.ProfilePublicTotalsParams{UserID: userID, Since: since})
		if err != nil {
			return aggregates{}, fmt.Errorf("querying public totals: %w", err)
		}
		has, err := s.st.StatsUserHasDevices(ctx, userID)
		if err != nil {
			return aggregates{}, fmt.Errorf("checking devices: %w", err)
		}
		var credited []db.ProfilePublicCreditedRow
		if has {
			credited, err = s.st.ProfilePublicCredited(ctx, db.ProfilePublicCreditedParams{UserID: userID, Since: since})
			if err != nil {
				return aggregates{}, fmt.Errorf("querying public credited totals: %w", err)
			}
			if credited == nil {
				credited = []db.ProfilePublicCreditedRow{} // has devices: zeros, not null
			}
		}
		v, err := versionOf(rows, credited)
		if err != nil {
			return aggregates{}, err
		}
		return aggregates{public: buildPublic(rows, credited), version: v}, nil
	})
}

// versionOf hashes the all-time rows, raw and credited. The last-7-days
// figures are left out: they shift as time passes, and a version that moved
// without new visible activity would hint at hidden activity.
func versionOf(rows []db.ProfilePublicTotalsRow, credited []db.ProfilePublicCreditedRow) (string, error) {
	type key struct {
		Project, Language, RepoURL string
		Seconds                    int64
	}
	keys := make([]key, len(rows))
	for i, r := range rows {
		keys[i] = key{r.Project, r.Language, r.RepoUrl, r.Seconds}
	}
	slices.SortFunc(keys, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.Project, b.Project), cmp.Compare(a.Language, b.Language))
	})
	type creditedKey struct {
		Project string
		Seconds int64
	}
	ckeys := make([]creditedKey, len(credited))
	for i, c := range credited {
		ckeys[i] = creditedKey{c.Project, c.Seconds}
	}
	slices.SortFunc(ckeys, func(a, b creditedKey) int { return cmp.Compare(a.Project, b.Project) })
	var b []byte
	var err error
	if credited == nil {
		b, err = json.Marshal(keys) // unchanged for users without a device
	} else {
		b, err = json.Marshal(map[string]any{"raw": keys, "credited": ckeys})
	}
	if err != nil {
		return "", fmt.Errorf("encoding version: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16]), nil
}

// buildPublic folds per (project, language) rows and per-project credited
// rows into totals, the top projects and the language breakdown. A nil
// credited means the user has no device, so the credited fields stay null.
func buildPublic(rows []db.ProfilePublicTotalsRow, credited []db.ProfilePublicCreditedRow) Public {
	byProject := map[string]*PublicProject{}
	byLang := map[string]int64{}
	var p Public
	project := func(name, repoURL string) *PublicProject {
		pp, ok := byProject[name]
		if !ok {
			pp = &PublicProject{Name: name, RepoURL: repoURL}
			byProject[name] = pp
		}
		return pp
	}
	for _, r := range rows {
		pp := project(r.Project, r.RepoUrl)
		pp.TotalSeconds += r.Seconds
		pp.Last7DaysSeconds += r.RecentSeconds
		byLang[r.Language] += r.Seconds
		p.Totals.AllTimeSeconds += r.Seconds
		p.Totals.Last7DaysSeconds += r.RecentSeconds
	}
	if credited != nil {
		var all, recent int64
		for _, c := range credited {
			pp := project(c.Project, c.RepoUrl)
			sec, rec := c.Seconds, c.RecentSeconds
			pp.CreditedSeconds, pp.CreditedLast7DaysSeconds = &sec, &rec
			all += c.Seconds
			recent += c.RecentSeconds
		}
		p.Totals.CreditedAllTimeSeconds, p.Totals.CreditedLast7DaysSeconds = &all, &recent
		for _, pp := range byProject {
			if pp.CreditedSeconds == nil {
				pp.CreditedSeconds, pp.CreditedLast7DaysSeconds = new(int64), new(int64)
			}
		}
	}
	p.Projects = make([]PublicProject, 0, len(byProject))
	for _, pp := range byProject {
		p.Projects = append(p.Projects, *pp)
	}
	credit := func(pp PublicProject) int64 {
		if pp.CreditedSeconds == nil {
			return 0
		}
		return *pp.CreditedSeconds
	}
	slices.SortFunc(p.Projects, func(a, b PublicProject) int {
		return cmp.Or(cmp.Compare(b.TotalSeconds, a.TotalSeconds), cmp.Compare(credit(b), credit(a)), cmp.Compare(a.Name, b.Name))
	})
	if len(p.Projects) > TopProjects {
		p.Projects = p.Projects[:TopProjects]
	}
	p.Languages = make([]Item, 0, len(byLang))
	for name, sec := range byLang {
		p.Languages = append(p.Languages, Item{Name: name, Seconds: sec})
	}
	slices.SortFunc(p.Languages, func(a, b Item) int {
		if c := cmp.Compare(b.Seconds, a.Seconds); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return p
}

// Me returns the user's own profile settings.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (Me, error) {
	u, err := s.st.ProfileGetByID(ctx, userID)
	if store.IsNotFound(err) {
		return Me{}, ErrNotFound
	}
	if err != nil {
		return Me{}, fmt.Errorf("reading profile: %w", err)
	}
	return meFromRow(db.ProfileUpdateRow(u)), nil
}

func meFromRow(u db.ProfileUpdateRow) Me {
	return Me{
		Username: u.Username, DisplayName: u.DisplayName, AvatarURL: u.AvatarUrl,
		Timezone: u.Timezone, ProfilePublic: u.ProfilePublic, MemberSince: u.CreatedAt,
	}
}

// Update validates and applies p, audits the changes and, when the profile
// became private, tells the PrivacyListener.
func (s *Service) Update(ctx context.Context, userID uuid.UUID, p Patch) (Me, error) {
	p, err := s.validate(ctx, p)
	if err != nil {
		return Me{}, err
	}
	var before, after db.ProfileUpdateRow
	err = s.st.InTx(ctx, func(q *db.Queries) error {
		// Lock the row so concurrent updates are serialised and each audit
		// entry records the value it actually replaced.
		b, err := q.ProfileGetByIDForUpdate(ctx, userID)
		if store.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("reading profile: %w", err)
		}
		before = db.ProfileUpdateRow(b)
		after, err = q.ProfileUpdate(ctx, db.ProfileUpdateParams{
			ID:            userID,
			DisplayName:   optText(p.DisplayName),
			Timezone:      optText(p.Timezone),
			ProfilePublic: optBool(p.ProfilePublic),
		})
		if err != nil {
			return fmt.Errorf("updating profile: %w", err)
		}
		return audit(ctx, q, userID, before, after)
	})
	if err != nil {
		return Me{}, fmt.Errorf("updating profile: %w", err)
	}
	if before.ProfilePublic && !after.ProfilePublic && s.listener != nil {
		s.listener.ProfileMadePrivate(userID)
	}
	return meFromRow(after), nil
}

func (s *Service) validate(ctx context.Context, p Patch) (Patch, error) {
	if p.DisplayName == nil && p.Timezone == nil && p.ProfilePublic == nil {
		return p, ErrEmptyPatch
	}
	if p.DisplayName != nil {
		name, err := NormalizeDisplayName(*p.DisplayName)
		if err != nil {
			return p, err
		}
		p.DisplayName = &name
	}
	if p.Timezone != nil {
		if err := ValidateTimezone(*p.Timezone); err != nil {
			return p, err
		}
		// Postgres buckets days with AT TIME ZONE, so it must know the zone too.
		known, err := s.st.ProfileTimezoneKnown(ctx, *p.Timezone)
		if err != nil {
			return p, fmt.Errorf("checking timezone: %w", err)
		}
		if !known {
			return p, ErrInvalidTimezone
		}
	}
	return p, nil
}

type change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

func audit(ctx context.Context, q *db.Queries, userID uuid.UUID, before, after db.ProfileUpdateRow) error {
	changes := map[string]change{}
	if before.DisplayName != after.DisplayName {
		changes["display_name"] = change{before.DisplayName, after.DisplayName}
	}
	if before.Timezone != after.Timezone {
		changes["timezone"] = change{before.Timezone, after.Timezone}
	}
	if before.ProfilePublic != after.ProfilePublic {
		changes["profile_public"] = change{before.ProfilePublic, after.ProfilePublic}
	}
	if len(changes) == 0 {
		return nil
	}
	data, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorUserID: pgtype.UUID{Bytes: userID, Valid: true},
		Action:      "profile.update",
		SubjectType: "user",
		SubjectID:   userID.String(),
		Data:        data,
	}); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return nil
}

func optText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func optBool(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}
