package admin

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/submissions"
)

// Page size of submissions/{id}/evidence.
const (
	defaultEvidence = 1000
	maxEvidence     = 5000
)

var (
	errSubmissionNotFound = notFound("submission not found")
	errEvidenceGone       = httpapi.Errorf(http.StatusGone, nil, "the submission's raw evidence was deleted")
)

// Submission is a submission as admins see it: every field, the trust
// level at submission unmasked.
type Submission struct {
	ID      uuid.UUID `json:"id"`
	User    UserRef   `json:"user"`
	Project string    `json:"project"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	// Program is null without one; a deleted program keeps its slug and name.
	Program *ProgramRef `json:"program"`
	Status  string      `json:"status"`
	Error   string      `json:"error"`
	// TrustAtSubmission and HackatimeUserID are null when Hackatime wasn't
	// linked at submission.
	TrustAtSubmission *string `json:"trust_at_submission"`
	HackatimeUserID   *int64  `json:"hackatime_user_id"`
	Minutes           struct {
		Verified int32 `json:"verified"`
		Mismatch int32 `json:"mismatch"`
		Unbacked int32 `json:"unbacked"`
	} `json:"minutes"`
	CountedSeconds int64      `json:"counted_seconds"`
	RawSeconds     int64      `json:"raw_seconds"`
	PartsVersion   int32      `json:"parts_version"`
	Parts          int64      `json:"parts"`
	RawDeleted     bool       `json:"raw_deleted"`
	CreatedAt      time.Time  `json:"created_at"`
	VerifiedAt     *time.Time `json:"verified_at"`
	DecidedAt      *time.Time `json:"decided_at"`
	WithdrawnAt    *time.Time `json:"withdrawn_at"`
	// Decision is the latest decision, null without one.
	Decision *DecisionJSON `json:"decision"`
}

// ProgramRef names a program.
type ProgramRef struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// DecisionJSON is one decision; by is null once the reviewer's account is gone.
type DecisionJSON struct {
	ID              int64     `json:"id"`
	Value           string    `json:"value"`
	ApprovedSeconds *int64    `json:"approved_seconds"`
	Reason          string    `json:"reason"`
	By              *UserRef  `json:"by"`
	At              time.Time `json:"at"`
}

// submissionsOf reads submissions (by user or by id) with their decisions,
// newest decision first per submission.
func (s *Service) submissionsOf(ctx context.Context, arg db.AdminSubmissionsParams) ([]db.AdminSubmissionsRow, map[uuid.UUID][]DecisionJSON, error) {
	rows, err := s.q.AdminSubmissions(ctx, arg)
	if err != nil {
		return nil, nil, fmt.Errorf("reading submissions: %w", err)
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	decisions := map[uuid.UUID][]DecisionJSON{}
	if len(ids) == 0 {
		return rows, decisions, nil
	}
	ds, err := s.q.AdminSubmissionDecisions(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("reading decisions: %w", err)
	}
	for _, d := range ds {
		decisions[d.SubmissionID] = append(decisions[d.SubmissionID], DecisionJSON{
			ID: d.ID, Value: d.Value, ApprovedSeconds: optInt8(d.ApprovedSeconds), Reason: d.Reason,
			By: userRef(d.DecidedByPublicID, d.DecidedByUsername), At: d.DecidedAt.UTC(),
		})
	}
	return rows, decisions, nil
}

func toSubmission(r db.AdminSubmissionsRow, owner UserRef, decisions []DecisionJSON) Submission {
	s := Submission{
		ID: r.ID, User: owner, Project: r.Project, From: day(r.FromDay), To: day(r.ToDay),
		Status: r.Status, Error: r.Error, TrustAtSubmission: optText(r.TrustAtSubmission),
		HackatimeUserID: optInt8(r.HackatimeUserID), CountedSeconds: r.CountedSeconds, RawSeconds: r.RawSeconds,
		PartsVersion: r.PartsVersion, Parts: r.Parts, RawDeleted: r.PartsDeleted > 0,
		CreatedAt: r.CreatedAt.UTC(), VerifiedAt: utcPtr(r.VerifiedAt), DecidedAt: utcPtr(r.DecidedAt),
		WithdrawnAt: utcPtr(r.WithdrawnAt),
	}
	s.Minutes.Verified, s.Minutes.Mismatch, s.Minutes.Unbacked = r.VerifiedMinutes, r.MismatchMinutes, r.UnbackedMinutes
	if r.HasProgram {
		s.Program = &ProgramRef{Slug: r.ProgramSlug, Name: r.ProgramName}
	}
	if len(decisions) > 0 {
		d := decisions[0]
		s.Decision = &d
	}
	return s
}

func day(d pgtype.Date) string { return d.Time.Format(time.DateOnly) }

// Submissions answers user/submissions: every submission of the user, any
// status, newest first.
func (s *Service) Submissions(ctx context.Context, userID uuid.UUID) ([]Submission, error) {
	rows, decisions, err := s.submissionsOf(ctx, db.AdminSubmissionsParams{UserID: pgtype.UUID{Bytes: userID, Valid: true}})
	if err != nil {
		return nil, err
	}
	out := make([]Submission, len(rows))
	for i, r := range rows {
		out[i] = toSubmission(r, UserRef{}, decisions[r.ID])
	}
	if len(rows) > 0 {
		owner, err := s.owner(ctx, userID)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].User = owner
		}
	}
	return out, nil
}

// owner names a submission's user.
func (s *Service) owner(ctx context.Context, userID uuid.UUID) (UserRef, error) {
	rows, err := s.q.AdminSubmissionOwner(ctx, userID)
	if err != nil {
		return UserRef{}, fmt.Errorf("reading submitter: %w", err)
	}
	return UserRef{ID: rows.PublicID, Username: rows.Username}, nil
}

// SubmissionDetail is submissions/{id}.
type SubmissionDetail struct {
	Submission Submission      `json:"submission"`
	Packet     json.RawMessage `json:"packet"`
	Decisions  []DecisionJSON  `json:"decisions"`
	Parts      []PartInfo      `json:"parts"`
	Minutes    []FrozenMinute  `json:"minutes"`
	userID     uuid.UUID       // the audit subject
}

// PartInfo is a stored part without its data.
type PartInfo struct {
	DeviceID     uuid.UUID  `json:"device_id"`
	DeviceName   string     `json:"device_name"`
	Bytes        int32      `json:"bytes"`
	UploadedAt   time.Time  `json:"uploaded_at"`
	RawDeletedAt *time.Time `json:"raw_deleted_at"`
}

// FrozenMinute is one frozen row of a submission with the re-check's
// result: result is null while verifying; server is the replay's row on a
// mismatch, else null.
type FrozenMinute struct {
	DeviceID     uuid.UUID     `json:"device_id"`
	Minute       time.Time     `json:"minute"`
	Label        string        `json:"label"`
	Credited     int16         `json:"credited"`
	Cuts         []string      `json:"cuts"`
	Flags        []string      `json:"flags"`
	RulesVersion string        `json:"rules_version"`
	Key          *MinuteKey    `json:"key"`
	Result       *string       `json:"result"`
	Reason       string        `json:"reason"`
	Server       *ServerResult `json:"server"`
}

// ServerResult is the server's replay of a mismatched row.
type ServerResult struct {
	Label    string   `json:"label"`
	Credited int16    `json:"credited"`
	Cuts     []string `json:"cuts"`
}

// Submission answers submissions/{id}: ErrNotFound when no submission has
// the id.
func (s *Service) Submission(ctx context.Context, id uuid.UUID) (SubmissionDetail, error) {
	rows, decisions, err := s.submissionsOf(ctx, db.AdminSubmissionsParams{ID: pgtype.UUID{Bytes: id, Valid: true}})
	if err != nil {
		return SubmissionDetail{}, err
	}
	if len(rows) == 0 {
		return SubmissionDetail{}, ErrNotFound
	}
	r := rows[0]
	owner, err := s.owner(ctx, r.UserID)
	if err != nil {
		return SubmissionDetail{}, err
	}
	parts, err := s.q.AdminSubmissionParts(ctx, id)
	if err != nil {
		return SubmissionDetail{}, fmt.Errorf("reading parts: %w", err)
	}
	minutes, err := s.q.AdminSubmissionMinutes(ctx, id)
	if err != nil {
		return SubmissionDetail{}, fmt.Errorf("reading frozen minutes: %w", err)
	}
	d := SubmissionDetail{
		Submission: toSubmission(r, owner, decisions[id]), Packet: json.RawMessage("null"),
		Decisions: decisions[id], Parts: make([]PartInfo, len(parts)), Minutes: make([]FrozenMinute, len(minutes)),
		userID: r.UserID,
	}
	if r.Packet != nil {
		d.Packet = json.RawMessage(r.Packet)
	}
	if d.Decisions == nil {
		d.Decisions = []DecisionJSON{}
	}
	for i, p := range parts {
		d.Parts[i] = PartInfo{DeviceID: p.DeviceID, DeviceName: p.DeviceName, Bytes: p.Bytes, UploadedAt: p.UploadedAt.UTC(), RawDeletedAt: utcPtr(p.RawDeletedAt)}
	}
	for i, m := range minutes {
		fm := FrozenMinute{
			DeviceID: m.DeviceID, Minute: m.Minute.UTC(), Label: m.Label, Credited: m.Credited,
			Cuts: nonNil(m.Cuts), Flags: nonNil(m.Flags), RulesVersion: m.RulesVersion, Reason: m.Reason,
		}
		if m.ComputedAt != nil && m.Snapshot.Valid {
			fm.Key = &MinuteKey{ComputedAt: m.ComputedAt.UTC(), Snapshot: m.Snapshot.Int64}
		}
		if m.Result.Valid {
			res := m.Result.String
			fm.Result = &res
		}
		if m.ServerLabel.Valid {
			fm.Server = &ServerResult{Label: m.ServerLabel.String, Credited: m.ServerCredited.Int16, Cuts: nonNil(m.ServerCuts)}
		}
		d.Minutes[i] = fm
	}
	return d, nil
}

// submissionID parses the {id} path value: not a UUID is 404.
func submissionID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, errSubmissionNotFound
	}
	return id, nil
}

func (h *Handler) submission(w http.ResponseWriter, r *http.Request, c *call) error {
	id, err := submissionID(r)
	if err != nil {
		return err
	}
	d, err := h.svc.Submission(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		return errSubmissionNotFound
	}
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, d, d.userID)
}

// submissionOwner returns the user a submission belongs to (404 when no
// submission has the id).
func (h *Handler) submissionOwner(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	rows, err := h.svc.q.AdminSubmissions(ctx, db.AdminSubmissionsParams{ID: pgtype.UUID{Bytes: id, Valid: true}})
	if err != nil {
		return uuid.Nil, fmt.Errorf("reading submission: %w", err)
	}
	if len(rows) == 0 {
		return uuid.Nil, errSubmissionNotFound
	}
	return rows[0].UserID, nil
}

// evidenceError maps the submissions package's errors.
func evidenceError(err error) error {
	switch {
	case errors.Is(err, submissions.ErrNotFound):
		return errSubmissionNotFound
	case errors.Is(err, submissions.ErrEvidenceDeleted):
		return errEvidenceGone
	case errors.Is(err, submissions.ErrBusy):
		return httpapi.Errorf(http.StatusServiceUnavailable, err, "%s", err.Error())
	}
	return err
}

func (h *Handler) submissionEvidence(w http.ResponseWriter, r *http.Request, c *call) error {
	ctx := r.Context()
	id, err := submissionID(r)
	if err != nil {
		return err
	}
	owner, err := h.submissionOwner(ctx, id)
	if err != nil {
		return err
	}
	parts, err := h.svc.q.AdminSubmissionParts(ctx, id)
	if err != nil {
		return fmt.Errorf("reading parts: %w", err)
	}
	var device uuid.UUID
	switch {
	case c.p.present("part"):
		if device, err = uuid.Parse(c.p["part"]); err != nil {
			return httpapi.Errorf(http.StatusBadRequest, nil, "part must be a device id")
		}
	case len(parts) == 1:
		device = parts[0].DeviceID
	case len(parts) == 0:
		return notFound("the submission has no parts")
	default:
		return httpapi.Errorf(http.StatusBadRequest, nil, "the submission has %d parts; name one with part=<device id>", len(parts))
	}
	limit := c.p.intParam("limit", defaultEvidence, 1, maxEvidence)
	offset := c.p.intParam("offset", 0, 0, math.MaxInt32)
	// The read is audited before the part is decoded: whatever happens
	// next, the admin asked to see the user's raw rows.
	if err := c.audit(ctx, owner); err != nil {
		return err
	}
	rows, total, err := h.svc.ev.PartRows(ctx, id, device, int(offset), int(limit))
	if errors.Is(err, submissions.ErrNotFound) {
		return notFound("the submission has no part from that device")
	}
	if err != nil {
		return evidenceError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		SubmissionID uuid.UUID                 `json:"submission_id"`
		DeviceID     uuid.UUID                 `json:"device_id"`
		Rows         []submissions.EvidenceRow `json:"rows"`
		Total        int                       `json:"total"`
		Page
	}{id, device, rows, total, Page{Limit: limit, Offset: offset, HasMore: offset+limit < int64(total)}})
	return nil
}

// errExportsBusy refuses a bundle export while every export slot
// (HandlerOptions.BundleExports) is taken.
var errExportsBusy = httpapi.Errorf(http.StatusServiceUnavailable, nil, "too many submission bundles are being exported; try again in a minute")

func (h *Handler) submissionBundle(w http.ResponseWriter, r *http.Request, c *call) error {
	ctx := r.Context()
	id, err := submissionID(r)
	if err != nil {
		return err
	}
	owner, err := h.submissionOwner(ctx, id)
	if err != nil {
		return err
	}
	if err := h.svc.ev.CheckBundle(ctx, id); err != nil {
		return evidenceError(err)
	}
	select {
	case h.exports <- struct{}{}:
		defer func() { <-h.exports }()
	default:
		w.Header().Set("Retry-After", "60")
		return errExportsBusy
	}
	if err := c.audit(ctx, owner); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="submission-%s.json.gz"`, id))
	w.WriteHeader(http.StatusOK)
	zw := gzip.NewWriter(&deadlineWriter{w: w, rc: http.NewResponseController(w), timeout: h.exportWriteTimeout})
	err = h.svc.ev.WriteBundle(ctx, id, zw)
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		// The status is sent: cut the connection so the client sees a
		// truncated gzip stream rather than a bundle that looks whole.
		slog.ErrorContext(ctx, "exporting a submission bundle failed", "err", err, "submission_id", id)
		panic(http.ErrAbortHandler)
	}
	return nil
}

// deadlineWriter gives every write to a bundle download its own deadline,
// so a client that stops reading loses its export slot (and connection)
// instead of holding it for as long as the connection stays open.
type deadlineWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if err := d.rc.SetWriteDeadline(time.Now().Add(d.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, fmt.Errorf("setting write deadline: %w", err)
	}
	return d.w.Write(p)
}
