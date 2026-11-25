package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Page sizes of the Logbook calls.
const (
	defaultMinutes = 1000
	maxMinutes     = 5000
	defaultAudit   = 100
	maxAudit       = 1000
)

// Device is one device of user/devices with its evidence chain.
type Device struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	OS         string     `json:"os"`
	PublicKey  []byte     `json:"public_key"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Chain      Chain      `json:"chain"`
}

// Chain describes a device's stored evidence days (ADR 0010, ADR 0019).
type Chain struct {
	// DaysSealed counts the stored days; DaysWithBlock those with block
	// fields, the only ones a submission can be checked against.
	DaysSealed    int `json:"days_sealed"`
	DaysWithBlock int `json:"days_with_block"`
	// FirstDay and LastDay are null without any stored day.
	FirstDay *string `json:"first_day"`
	LastDay  *string `json:"last_day"`
	// GapDays counts the days between the first and last with no record;
	// Gaps lists them as inclusive ranges.
	GapDays int        `json:"gap_days"`
	Gaps    []DayRange `json:"gaps"`
	// RefusedUploads are the conflicting records the server refused: a
	// different, validly signed record for a day already stored.
	RefusedUploads []RefusedUpload `json:"refused_uploads"`
}

// DayRange is an inclusive range of UTC days.
type DayRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RefusedUpload is one evidence_conflict entry: the day, when, and both
// records' hashes and seq ranges as the audit row holds them.
type RefusedUpload struct {
	Day    string          `json:"day"`
	At     time.Time       `json:"at"`
	Detail json.RawMessage `json:"detail"`
}

// Devices answers user/devices for u.
func (s *Service) Devices(ctx context.Context, u UserRow) ([]Device, error) {
	devs, err := s.q.AdminDevices(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("reading devices: %w", err)
	}
	days, err := s.q.AdminEvidenceDays(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("reading evidence days: %w", err)
	}
	ids := make([]string, len(devs))
	for i, d := range devs {
		ids[i] = d.ID.String()
	}
	conflicts, err := s.q.AdminEvidenceConflicts(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading refused uploads: %w", err)
	}
	byDevice := map[uuid.UUID][]db.AdminEvidenceDaysRow{}
	for _, d := range days {
		byDevice[d.DeviceID] = append(byDevice[d.DeviceID], d)
	}
	refused := map[string][]RefusedUpload{}
	for _, c := range conflicts {
		refused[c.DeviceID] = append(refused[c.DeviceID], RefusedUpload{Day: c.Day, At: c.CreatedAt.UTC(), Detail: json.RawMessage(c.Data)})
	}
	out := make([]Device, len(devs))
	for i, d := range devs {
		out[i] = Device{
			ID: d.ID, Name: d.Name, OS: d.Os, PublicKey: d.PublicKey, CreatedAt: d.CreatedAt.UTC(),
			LastSeenAt: utcPtr(d.LastSeenAt), RevokedAt: utcPtr(d.RevokedAt),
			Chain: chain(byDevice[d.ID], refused[d.ID.String()]),
		}
	}
	return out, nil
}

// chain summarises one device's stored days (oldest first).
func chain(days []db.AdminEvidenceDaysRow, refused []RefusedUpload) Chain {
	c := Chain{DaysSealed: len(days), Gaps: []DayRange{}, RefusedUploads: refused}
	if c.RefusedUploads == nil {
		c.RefusedUploads = []RefusedUpload{}
	}
	for i, d := range days {
		if d.HasBlock {
			c.DaysWithBlock++
		}
		if i == 0 {
			continue
		}
		prev := days[i-1].Day.Time
		if next := prev.AddDate(0, 0, 1); next.Before(d.Day.Time) {
			last := d.Day.Time.AddDate(0, 0, -1)
			c.Gaps = append(c.Gaps, DayRange{From: next.Format(time.DateOnly), To: last.Format(time.DateOnly)})
			c.GapDays += int(last.Sub(next).Hours()/24) + 1
		}
	}
	if len(days) > 0 {
		first, last := days[0].Day.Time.Format(time.DateOnly), days[len(days)-1].Day.Time.Format(time.DateOnly)
		c.FirstDay, c.LastDay = &first, &last
	}
	return c
}

func (h *Handler) userDevices(w http.ResponseWriter, r *http.Request, c *call) error {
	u, err := h.user(r.Context(), c)
	if err != nil {
		return err
	}
	devs, err := h.svc.Devices(r.Context(), u)
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, map[string]any{"user_id": u.PublicID, "devices": devs}, u.ID)
}

// Minute is one credited row of user/minutes.
type Minute struct {
	DeviceID     uuid.UUID `json:"device_id"`
	Minute       time.Time `json:"minute"`
	Project      string    `json:"project"`
	Label        string    `json:"label"`
	Credited     int16     `json:"credited"`
	Cuts         []string  `json:"cuts"`
	Flags        []string  `json:"flags"`
	RulesVersion string    `json:"rules_version"`
	// Key is the row's computation key (ADR 0017), null for rows stored
	// before it.
	Key       *MinuteKey `json:"key"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// MinuteKey is a computation key: the run's window end and the event
// log's high-water mark.
type MinuteKey struct {
	ComputedAt time.Time `json:"computed_at"`
	Snapshot   int64     `json:"snapshot"`
}

// MinutesQuery is a parsed user/minutes request.
type MinutesQuery struct {
	Range         timeRange
	Project       string
	Limit, Offset int64
}

// Page is a page's position.
type Page struct {
	Limit   int64 `json:"limit"`
	Offset  int64 `json:"offset"`
	HasMore bool  `json:"has_more"`
}

// Minutes answers user/minutes for u: one page by (minute, device,
// project), and whether more follow.
func (s *Service) Minutes(ctx context.Context, u UserRow, q MinutesQuery) ([]Minute, Page, error) {
	rows, err := s.q.AdminCreditedMinutes(ctx, db.AdminCreditedMinutesParams{
		UserID: u.ID, FromTime: q.Range.From, ToTime: q.Range.To, Project: filter(q.Project),
		MaxRows: int32(q.Limit + 1), SkipRows: int32(q.Offset), //nolint:gosec // both clamped
	})
	if err != nil {
		return nil, Page{}, fmt.Errorf("reading credited minutes: %w", err)
	}
	page := Page{Limit: q.Limit, Offset: q.Offset, HasMore: int64(len(rows)) > q.Limit}
	if page.HasMore {
		rows = rows[:q.Limit]
	}
	out := make([]Minute, len(rows))
	for i, r := range rows {
		out[i] = Minute{
			DeviceID: r.DeviceID, Minute: r.Minute.UTC(), Project: r.Project, Label: r.Label, Credited: r.Credited,
			Cuts: nonNil(r.Cuts), Flags: nonNil(r.Flags), RulesVersion: r.RulesVersion, UpdatedAt: r.UpdatedAt.UTC(),
		}
		if r.ComputedAt != nil && r.Snapshot.Valid {
			out[i].Key = &MinuteKey{ComputedAt: r.ComputedAt.UTC(), Snapshot: r.Snapshot.Int64}
		}
	}
	return out, page, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (h *Handler) userMinutes(w http.ResponseWriter, r *http.Request, c *call) error {
	u, err := h.user(r.Context(), c)
	if err != nil {
		return err
	}
	rg, bad := c.p.applyTimeRange(h.svc.now())
	if bad != "" {
		return unprocessable("invalid " + bad)
	}
	q := MinutesQuery{
		Range: rg, Limit: c.p.intParam("limit", defaultMinutes, 1, maxMinutes),
		Offset: c.p.intParam("offset", 0, 0, math.MaxInt32-maxMinutes),
	}
	if c.p.present("project") {
		q.Project = c.p["project"]
	}
	minutes, page, err := h.svc.Minutes(r.Context(), u, q)
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, struct {
		UserID  int64    `json:"user_id"`
		Minutes []Minute `json:"minutes"`
		Page
	}{u.PublicID, minutes, page}, u.ID)
}

func (h *Handler) userSubmissions(w http.ResponseWriter, r *http.Request, c *call) error {
	u, err := h.user(r.Context(), c)
	if err != nil {
		return err
	}
	subs, err := h.svc.Submissions(r.Context(), u.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, map[string]any{"user_id": u.PublicID, "submissions": subs}, u.ID)
}

// AuditEntry is one user/audit_log entry.
type AuditEntry struct {
	ID          int64           `json:"id"`
	Action      string          `json:"action"`
	Actor       *UserRef        `json:"actor"`
	SubjectType string          `json:"subject_type"`
	SubjectID   string          `json:"subject_id"`
	Data        json.RawMessage `json:"data"`
	CreatedAt   time.Time       `json:"created_at"`
}

// UserRef names a user by public id and username.
type UserRef struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func userRef(id pgtype.Int8, username string) *UserRef {
	if !id.Valid {
		return nil
	}
	return &UserRef{ID: id.Int64, Username: username}
}

// AuditLog answers user/audit_log for u: entries where u is the actor or
// the subject (u, one of u's devices or submissions), newest first.
// Entries are read below beforeID (0 means "now"), which the answer gives
// back, so later pages of the same listing see the same entries.
func (s *Service) AuditLog(ctx context.Context, u UserRow, limit, offset, beforeID int64) ([]AuditEntry, Page, int64, error) {
	if beforeID <= 0 {
		next, err := s.q.AdminAuditNextID(ctx)
		if err != nil {
			return nil, Page{}, 0, fmt.Errorf("reading audit log: %w", err)
		}
		beforeID = next
	}
	rows, err := s.q.AdminAuditLog(ctx, db.AdminAuditLogParams{
		BeforeID: beforeID, UserID: u.ID, SubjectID: u.ID.String(),
		MaxRows: int32(limit + 1), SkipRows: int32(offset), //nolint:gosec // both clamped
	})
	if err != nil {
		return nil, Page{}, 0, fmt.Errorf("reading audit log: %w", err)
	}
	page := Page{Limit: limit, Offset: offset, HasMore: int64(len(rows)) > limit}
	if page.HasMore {
		rows = rows[:limit]
	}
	out := make([]AuditEntry, len(rows))
	for i, r := range rows {
		out[i] = AuditEntry{
			ID: r.ID, Action: r.Action, Actor: userRef(r.ActorPublicID, r.ActorUsername),
			SubjectType: r.SubjectType, SubjectID: r.SubjectID, Data: json.RawMessage(r.Data), CreatedAt: r.CreatedAt.UTC(),
		}
	}
	return out, page, beforeID, nil
}

func (h *Handler) userAuditLog(w http.ResponseWriter, r *http.Request, c *call) error {
	u, err := h.user(r.Context(), c)
	if err != nil {
		return err
	}
	limit := c.p.intParam("limit", defaultAudit, 1, maxAudit)
	offset := c.p.intParam("offset", 0, 0, math.MaxInt32-maxAudit)
	before := c.p.intParam("before_id", 0, 0, math.MaxInt64)
	entries, page, before, err := h.svc.AuditLog(r.Context(), u, limit, offset, before)
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, struct {
		UserID   int64        `json:"user_id"`
		BeforeID int64        `json:"before_id"`
		Entries  []AuditEntry `json:"entries"`
		Page
	}{u.PublicID, before, entries, page}, u.ID)
}
