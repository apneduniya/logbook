package programapi

import (
	"context"
	"net/url"
	"time"
)

// MeBody is authenticated/me (Api::V1::Authenticated::MeController): id is
// the public id. slack_id is always null (Logbook doesn't know it);
// github_username is the one Hackatime reported when the user linked it.
// Logbook has no "verified confidential apps", so the trust level is always
// the masked one.
type MeBody struct {
	ID             int64       `json:"id"`
	Emails         []string    `json:"emails"`
	SlackID        *string     `json:"slack_id"`
	GitHubUsername *string     `json:"github_username"`
	TrustFactor    TrustFactor `json:"trust_factor"`
	Logbook        Extension   `json:"logbook"`
}

// Me answers authenticated/me for u.
func (s *Service) Me(ctx context.Context, u User) (MeBody, error) {
	link, err := s.Link(ctx, u.ID)
	if err != nil {
		return MeBody{}, err
	}
	emails := []string{}
	if u.Email != "" {
		emails = append(emails, u.Email)
	}
	verified := link.Trust.Verified
	return MeBody{
		ID: u.PublicID, Emails: emails, GitHubUsername: link.GitHubUsername,
		TrustFactor: trustFactor(link.Trust),
		Logbook:     Extension{Username: u.Username, HackatimeID: link.HackatimeID, TrustVerified: &verified},
	}, nil
}

// TrustBody is users/{id}/trust_factor.
type TrustBody struct {
	TrustLevel string    `json:"trust_level"`
	TrustValue int       `json:"trust_value"`
	Logbook    Extension `json:"logbook"`
}

// TrustFactor answers users/{id}/trust_factor for u.
func (s *Service) TrustFactor(ctx context.Context, u User) (TrustBody, error) {
	link, err := s.Link(ctx, u.ID)
	if err != nil {
		return TrustBody{}, err
	}
	verified := link.Trust.Verified
	return TrustBody{TrustLevel: link.Trust.Level, TrustValue: link.Trust.Value, Logbook: Extension{TrustVerified: &verified}}, nil
}

// AuthProject is one authenticated/projects row (the project picker):
// Hackatime's five fields plus credited_seconds, raw_seconds and repo_url.
type AuthProject struct {
	Name                string   `json:"name"`
	TotalSeconds        int64    `json:"total_seconds"`
	MostRecentHeartbeat *string  `json:"most_recent_heartbeat"`
	Languages           []string `json:"languages"`
	Archived            bool     `json:"archived"`
	CreditedSeconds     int64    `json:"credited_seconds"`
	RawSeconds          int64    `json:"raw_seconds"`
	RepoURL             *string  `json:"repo_url"`
}

// AuthProjectsQuery reads authenticated/projects' parameters: all time by
// default (Hackatime's default_stats_start: 0), projects, include_archived
// and raw.
func AuthProjectsQuery(q url.Values, now time.Time) ProjectQuery {
	return ProjectQuery{
		Range:           projectStatsRange(q, time.Unix(0, 0).UTC(), now),
		Names:           trimmedList(q, "projects"),
		IncludeArchived: isTrue(q, "include_archived"),
		Raw:             isTrue(q, "raw"),
	}
}

// AuthProjects answers authenticated/projects for the token's user.
func (s *Service) AuthProjects(ctx context.Context, u User, q ProjectQuery) ([]AuthProject, error) {
	details, err := s.ProjectDetails(ctx, u.ID, q)
	if err != nil {
		return nil, err
	}
	out := make([]AuthProject, len(details))
	for i, d := range details {
		out[i] = AuthProject{
			Name: d.Name, TotalSeconds: d.TotalSeconds, MostRecentHeartbeat: d.MostRecentHeartbeat,
			Languages: d.Languages, Archived: d.Archived,
			CreditedSeconds: d.CreditedSeconds, RawSeconds: d.RawSeconds, RepoURL: d.RepoURL,
		}
	}
	return out, nil
}

// UserProjectsQuery reads the users/{id}/project and projects/details
// parameters: the last year by default (Hackatime's default_stats_start).
func UserProjectsQuery(q url.Values, now time.Time, names []string) ProjectQuery {
	return ProjectQuery{
		Range: projectStatsRange(q, now.UTC().AddDate(-1, 0, 0), now),
		Names: names,
		Raw:   isTrue(q, "raw"),
	}
}
