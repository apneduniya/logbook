package projects

import (
	"cmp"
	"context"
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

// ErrNotFound is returned when the user has no project with that name.
var ErrNotFound = errors.New("project not found")

// ErrEmptyPatch is returned when an update sets no fields.
var ErrEmptyPatch = errors.New("no fields to update")

// Querier is the subset of the generated queries the service reads with.
type Querier interface {
	ProjectsMinuteTotals(ctx context.Context, arg db.ProjectsMinuteTotalsParams) ([]db.ProjectsMinuteTotalsRow, error)
	ProjectsSettingsList(ctx context.Context, userID uuid.UUID) ([]db.Project, error)
	ProjectsSettingsGet(ctx context.Context, arg db.ProjectsSettingsGetParams) (db.Project, error)
}

// Store is what the service needs: reads plus transactions for updates.
// *store.Store satisfies it.
type Store interface {
	Querier
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
}

// Service reads and updates one user's projects.
type Service struct {
	st Store
}

// NewService returns a Service backed by st.
func NewService(st Store) *Service { return &Service{st: st} }

// Language is the credited time in one language within a project.
type Language struct {
	Name    string `json:"name"`
	Seconds int64  `json:"seconds"`
}

// Project is a project with its settings and all-time credited totals.
type Project struct {
	Name         string     `json:"name"`
	TotalSeconds int64      `json:"total_seconds"`
	LastActivity *time.Time `json:"last_activity"`
	Languages    []Language `json:"languages"`
	RepoURL      string     `json:"repo_url"`
	Hidden       bool       `json:"hidden"`
	Archived     bool       `json:"archived"`
}

// Patch holds the settings to change; nil fields are left as they are.
type Patch struct {
	RepoURL  *string
	Hidden   *bool
	Archived *bool
}

// Validate checks the patch before it touches the database.
func (p Patch) Validate() error {
	if p.RepoURL == nil && p.Hidden == nil && p.Archived == nil {
		return ErrEmptyPatch
	}
	if p.RepoURL != nil {
		return ValidateRepoURL(*p.RepoURL)
	}
	return nil
}

// List returns all of the user's projects, most recently active first.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Project, error) {
	totals, err := s.st.ProjectsMinuteTotals(ctx, db.ProjectsMinuteTotalsParams{UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("querying project totals: %w", err)
	}
	settings, err := s.st.ProjectsSettingsList(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("listing project settings: %w", err)
	}
	return merge(totals, settings), nil
}

// Get returns one of the user's projects or ErrNotFound.
func (s *Service) Get(ctx context.Context, userID uuid.UUID, name string) (Project, error) {
	totals, err := s.st.ProjectsMinuteTotals(ctx, db.ProjectsMinuteTotalsParams{
		UserID: userID, Project: pgtype.Text{String: name, Valid: true},
	})
	if err != nil {
		return Project{}, fmt.Errorf("querying project totals: %w", err)
	}
	var settings []db.Project
	row, err := s.st.ProjectsSettingsGet(ctx, db.ProjectsSettingsGetParams{UserID: userID, Name: name})
	switch {
	case err == nil:
		settings = append(settings, row)
	case !store.IsNotFound(err):
		return Project{}, fmt.Errorf("reading project settings: %w", err)
	}
	ps := merge(totals, settings)
	if len(ps) == 0 {
		return Project{}, ErrNotFound
	}
	return ps[0], nil
}

// Update applies p to the user's project and writes an audit log entry when
// anything changed. It returns ErrNotFound for projects the user doesn't have.
func (s *Service) Update(ctx context.Context, userID uuid.UUID, name string, p Patch) (Project, error) {
	if err := p.Validate(); err != nil {
		return Project{}, err
	}
	if _, err := s.Get(ctx, userID, name); err != nil {
		return Project{}, err
	}
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		before, err := q.ProjectsSettingsGet(ctx, db.ProjectsSettingsGetParams{UserID: userID, Name: name})
		if err != nil && !store.IsNotFound(err) {
			return fmt.Errorf("reading project settings: %w", err)
		}
		after, err := q.ProjectsSettingsUpsert(ctx, db.ProjectsSettingsUpsertParams{
			UserID: userID, Name: name,
			RepoUrl:  optText(p.RepoURL),
			Hidden:   optBool(p.Hidden),
			Archived: optBool(p.Archived),
		})
		if err != nil {
			return fmt.Errorf("saving project settings: %w", err)
		}
		changes := diff(before, after)
		if len(changes) == 0 {
			return nil
		}
		data, err := json.Marshal(map[string]any{"name": name, "changes": changes})
		if err != nil {
			return fmt.Errorf("encoding audit data: %w", err)
		}
		if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			ActorUserID: pgtype.UUID{Bytes: userID, Valid: true},
			Action:      "project.update",
			SubjectType: "project",
			SubjectID:   after.ID.String(),
			Data:        data,
		}); err != nil {
			return fmt.Errorf("writing audit log: %w", err)
		}
		return nil
	})
	if err != nil {
		return Project{}, fmt.Errorf("updating project: %w", err)
	}
	return s.Get(ctx, userID, name)
}

// change records one field's old and new value for the audit log.
type change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// diff compares settings before (zero value if the row didn't exist) and after.
func diff(before, after db.Project) map[string]change {
	out := map[string]change{}
	if before.RepoUrl != after.RepoUrl {
		out["repo_url"] = change{before.RepoUrl, after.RepoUrl}
	}
	if before.Hidden != after.Hidden {
		out["hidden"] = change{before.Hidden, after.Hidden}
	}
	if before.Archived != after.Archived {
		out["archived"] = change{before.Archived, after.Archived}
	}
	return out
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

// merge joins per-language totals with settings rows into sorted projects.
func merge(totals []db.ProjectsMinuteTotalsRow, settings []db.Project) []Project {
	byName := map[string]*Project{}
	get := func(name string) *Project {
		p, ok := byName[name]
		if !ok {
			p = &Project{Name: name, Languages: []Language{}}
			byName[name] = p
		}
		return p
	}
	for _, t := range totals {
		p := get(t.Project)
		p.TotalSeconds += t.Seconds
		if t.Seconds > 0 {
			p.Languages = append(p.Languages, Language{Name: t.Language, Seconds: t.Seconds})
		}
		if last := t.LastMinute.Add(time.Minute); p.LastActivity == nil || last.After(*p.LastActivity) {
			p.LastActivity = &last
		}
	}
	for _, s := range settings {
		p := get(s.Name)
		p.RepoURL, p.Hidden, p.Archived = s.RepoUrl, s.Hidden, s.Archived
	}
	out := make([]Project, 0, len(byName))
	for _, p := range byName {
		slices.SortFunc(p.Languages, func(a, b Language) int {
			if c := cmp.Compare(b.Seconds, a.Seconds); c != 0 {
				return c
			}
			return cmp.Compare(a.Name, b.Name)
		})
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b Project) int {
		switch {
		case a.LastActivity == nil && b.LastActivity != nil:
			return 1
		case a.LastActivity != nil && b.LastActivity == nil:
			return -1
		case a.LastActivity != nil && !a.LastActivity.Equal(*b.LastActivity):
			return b.LastActivity.Compare(*a.LastActivity)
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}
