package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/submission"
)

// Program names the program a submission is addressed to.
type Program struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Decision is the latest decision on a submission.
type Decision struct {
	// Value is approved, adjusted or rejected.
	Value string `json:"value"`
	// ApprovedSeconds is set for adjusted.
	ApprovedSeconds *int64 `json:"approved_seconds"`
	Reason          string `json:"reason"`
	// By is the reviewer's username, null once their account is gone.
	By *string   `json:"by"`
	At time.Time `json:"at"`
}

// Submitter is what reviewers see of the person who submitted.
type Submitter struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	// HackatimeID is the linked Hackatime user id, null when not linked.
	HackatimeID *int64 `json:"hackatime_id"`
}

// Summary is a submission in a list (docs/api/submissions.md).
type Summary struct {
	ID      uuid.UUID `json:"id"`
	Project string    `json:"project"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	// User is set in a program's lists, never in the submitter's own.
	User    *Submitter `json:"user,omitempty"`
	Program *Program   `json:"program"`
	Status  string     `json:"status"`
	// Error says why a failed submission failed; only the submitter sees it.
	Error          string            `json:"error,omitempty"`
	CountedSeconds int64             `json:"counted_seconds"`
	RawSeconds     int64             `json:"raw_seconds"`
	Minutes        submission.Counts `json:"minutes"`
	Decision       *Decision         `json:"decision"`
	// ReviewURL is the review link, null once withdrawn.
	ReviewURL *string   `json:"review_url"`
	CreatedAt time.Time `json:"created_at"`
}

// Detail is a submission with its packet, null unless it is ready.
type Detail struct {
	Summary
	Packet *submission.Packet `json:"packet"`
}

// view says who a summary is for.
type view int

const (
	forSubmitter view = iota
	forProgram
)

// summaries builds the summaries of rows, reading their programs, latest
// decisions and (for programs) submitters in one query each.
func (s *Service) summaries(ctx context.Context, rows []db.Submission, v view) ([]Summary, error) {
	ids := make([]uuid.UUID, len(rows))
	var apps, users []uuid.UUID
	for i, r := range rows {
		ids[i] = r.ID
		if r.ProgramAppID.Valid {
			apps = append(apps, r.ProgramAppID.Bytes)
		}
		users = append(users, r.UserID)
	}
	programs, err := s.programs(ctx, apps)
	if err != nil {
		return nil, err
	}
	decisions, err := s.decisions(ctx, ids)
	if err != nil {
		return nil, err
	}
	var submitters map[uuid.UUID]Submitter
	if v == forProgram {
		if submitters, err = s.submitters(ctx, users); err != nil {
			return nil, err
		}
	}
	out := make([]Summary, len(rows))
	for i, r := range rows {
		sum := Summary{
			ID: r.ID, Project: r.Project, From: day(r.FromDay), To: day(r.ToDay), Status: r.Status,
			CountedSeconds: r.CountedSeconds, RawSeconds: r.RawSeconds,
			Minutes: submission.Counts{
				Verified: int(r.VerifiedMinutes), Mismatch: int(r.MismatchMinutes), Unbacked: int(r.UnbackedMinutes),
			},
			Decision: decisions[r.ID], CreatedAt: r.CreatedAt.UTC(),
		}
		if r.ProgramAppID.Valid {
			p := programs[r.ProgramAppID.Bytes]
			sum.Program = &p
		}
		if v == forSubmitter {
			sum.Error = r.Error
		} else {
			u := submitters[r.UserID]
			sum.User = &u
		}
		if r.Status != StatusWithdrawn {
			link, err := s.reviewURL(r)
			if err != nil {
				return nil, err
			}
			sum.ReviewURL = &link
		}
		out[i] = sum
	}
	return out, nil
}

func (s *Service) programs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Program, error) {
	out := map[uuid.UUID]Program{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.st.SubmissionPrograms(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading programs: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = Program{Slug: r.Slug, Name: r.Name}
	}
	return out, nil
}

func (s *Service) decisions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*Decision, error) {
	out := map[uuid.UUID]*Decision{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.st.SubmissionLatestDecisions(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading decisions: %w", err)
	}
	for _, r := range rows {
		d := &Decision{Value: r.Value, Reason: r.Reason, At: r.DecidedAt.UTC()}
		if r.ApprovedSeconds.Valid {
			v := r.ApprovedSeconds.Int64
			d.ApprovedSeconds = &v
		}
		if r.DecidedByUsername != "" {
			by := r.DecidedByUsername
			d.By = &by
		}
		out[r.SubmissionID] = d
	}
	return out, nil
}

func (s *Service) submitters(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Submitter, error) {
	out := map[uuid.UUID]Submitter{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.st.SubmissionSubmitters(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading submitters: %w", err)
	}
	for _, r := range rows {
		u := Submitter{Username: r.Username, DisplayName: r.DisplayName}
		if r.HackatimeUserID.Valid {
			id := r.HackatimeUserID.Int64
			u.HackatimeID = &id
		}
		out[r.ID] = u
	}
	return out, nil
}

// detail is a submission's summary plus its packet as served.
func (s *Service) detail(ctx context.Context, sub db.Submission, v view) (Detail, error) {
	sums, err := s.summaries(ctx, []db.Submission{sub}, v)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Summary: sums[0]}
	if d.ReviewURL != nil {
		d.Packet, err = s.servePacket(ctx, sub, *d.ReviewURL)
	}
	return d, err
}

// servePacket returns a ready submission's stored packet with what is
// filled in when it is served: the submitter's trust level now (masked)
// and when it was checked, the justification with the link, and the day
// the raw rows are deleted. It is nil unless the submission is ready.
func (s *Service) servePacket(ctx context.Context, sub db.Submission, link string) (*submission.Packet, error) {
	if sub.Status != StatusReady || sub.Packet == nil {
		return nil, nil
	}
	var p submission.Packet
	if err := json.Unmarshal(sub.Packet, &p); err != nil {
		return nil, fmt.Errorf("decoding packet of %s: %w", sub.ID, err)
	}
	t, err := readTrust(ctx, s.st, sub.UserID)
	if err != nil {
		return nil, err
	}
	p.Trust.Now = maskTrust(t.Level, t.Linked)
	if t.CheckedAt != nil {
		at := t.CheckedAt.UTC()
		p.Trust.CheckedAt = &at
	}
	p.Justification = submission.Justification(p, link)
	p.RawRowsDeletedAfter = rawDeletedAfter(sub).Format(time.DateOnly)
	return &p, nil
}

// Mine lists the user's submissions, newest first.
func (s *Service) Mine(ctx context.Context, user uuid.UUID) ([]Summary, error) {
	rows, err := s.st.SubmissionsOfUser(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("listing submissions: %w", err)
	}
	return s.summaries(ctx, rows, forSubmitter)
}

// MineOne returns one of the user's submissions with its packet.
func (s *Service) MineOne(ctx context.Context, user, id uuid.UUID) (Detail, error) {
	sub, err := s.st.SubmissionOfUser(ctx, db.SubmissionOfUserParams{ID: id, UserID: user})
	if err != nil {
		return Detail{}, notFound(err, ErrNotFound, "reading submission")
	}
	return s.detail(ctx, sub, forSubmitter)
}

// ProgramFilter narrows a program's list. Empty fields don't filter.
type ProgramFilter struct {
	// Status is a submission status.
	Status string
	// Decision is approved, adjusted, rejected, or none (no decision yet).
	Decision string
	// Limit (1 to MaxListLimit; nil is DefaultListLimit) and Offset page
	// the list.
	Limit  *int
	Offset int
}

// Page sizes of a program's list.
const (
	DefaultListLimit = 100
	MaxListLimit     = 500
	maxListOffset    = 1_000_000
)

var (
	statusFilters   = []string{StatusVerifying, StatusReady, StatusFailed, StatusWithdrawn}
	decisionFilters = []string{DecisionApproved, DecisionAdjusted, DecisionRejected, "none"}
)

func (f ProgramFilter) params(app uuid.UUID) (db.SubmissionsOfProgramParams, error) {
	p := db.SubmissionsOfProgramParams{AppID: pgtype.UUID{Bytes: app, Valid: true}, MaxRows: DefaultListLimit}
	if f.Status != "" {
		if !slices.Contains(statusFilters, f.Status) {
			return p, fmt.Errorf("%w: status must be verifying, ready, failed or withdrawn", ErrInvalid)
		}
		p.Status = pgtype.Text{String: f.Status, Valid: true}
	}
	if f.Decision != "" {
		if !slices.Contains(decisionFilters, f.Decision) {
			return p, fmt.Errorf("%w: decision must be approved, adjusted, rejected or none", ErrInvalid)
		}
		p.Decision = pgtype.Text{String: f.Decision, Valid: true}
	}
	if f.Limit != nil {
		if *f.Limit < 1 || *f.Limit > MaxListLimit {
			return p, fmt.Errorf("%w: limit must be 1 to %d", ErrInvalid, MaxListLimit)
		}
		p.MaxRows = int32(*f.Limit) //nolint:gosec // checked above
	}
	if f.Offset < 0 || f.Offset > maxListOffset {
		return p, fmt.Errorf("%w: offset must be 0 to %d", ErrInvalid, maxListOffset)
	}
	p.SkipRows = int32(f.Offset) //nolint:gosec // checked above
	return p, nil
}

// OfApp lists the submissions addressed to an app for one of its members
// (GET /api/v1/me/apps/{id}/submissions). It is ErrNotFound unless user is
// a member of the live app.
func (s *Service) OfApp(ctx context.Context, user, app uuid.UUID, f ProgramFilter) ([]Summary, error) {
	if _, err := s.members.GetApp(ctx, user, app); err != nil {
		if errors.Is(err, oauth.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("checking membership: %w", err)
	}
	return s.OfProgram(ctx, app, f)
}

// OfProgram lists the submissions addressed to app, for callers that have
// already authorised the app (its client-credentials token).
func (s *Service) OfProgram(ctx context.Context, app uuid.UUID, f ProgramFilter) ([]Summary, error) {
	p, err := f.params(app)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.SubmissionsOfProgram(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("listing program submissions: %w", err)
	}
	return s.summaries(ctx, rows, forProgram)
}

// OfProgramOne returns one submission addressed to app, with its packet.
func (s *Service) OfProgramOne(ctx context.Context, app, id uuid.UUID) (Detail, error) {
	sub, err := s.st.SubmissionOfProgram(ctx, db.SubmissionOfProgramParams{ID: id, AppID: pgtype.UUID{Bytes: app, Valid: true}})
	if err != nil {
		return Detail{}, notFound(err, ErrNotFound, "reading submission")
	}
	return s.detail(ctx, sub, forProgram)
}
