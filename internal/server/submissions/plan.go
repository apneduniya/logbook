package submissions

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/projectname"
	"github.com/apneduniya/logbook/pkg/submission"
)

// maxProjectLen bounds a project name, as credited minutes do.
const maxProjectLen = 256

// PlanQuery is GET /api/v1/me/submissions/plan's query.
type PlanQuery struct {
	Project, From, To string
	// Device is the agent's own device id, or "".
	Device string
	// Submission is the id of one of the user's submissions, or "": the
	// plan is then that submission's frozen rows, for adding a part.
	Submission string
}

// cleanProject normalises a project name as credited minutes store it.
func cleanProject(p string) (string, error) {
	p = projectname.Normalize(p)
	if p == "" || len(p) > maxProjectLen {
		return "", fmt.Errorf("%w: project is required (at most %d bytes)", ErrInvalid, maxProjectLen)
	}
	return p, nil
}

// parseRange is submission.ParseRange with its error as ErrInvalid.
func parseRange(from, to string) (submission.Range, error) {
	rg, err := submission.ParseRange(from, to)
	if err != nil {
		return rg, invalid(err)
	}
	return rg, nil
}

// Plan returns the rows the server holds for the user's project and range,
// from every device, for the agent to build a bundle. It is ErrInvalid when
// the range ends after a day some unrevoked device with rows in it hasn't
// sealed. With q.Submission it is that submission's frozen rows instead
// (submissionPlan).
func (s *Service) Plan(ctx context.Context, user uuid.UUID, q PlanQuery) (submission.Plan, error) {
	project, err := cleanProject(q.Project)
	if err != nil {
		return submission.Plan{}, err
	}
	rg, err := parseRange(q.From, q.To)
	if err != nil {
		return submission.Plan{}, err
	}
	var self uuid.UUID
	if q.Device != "" {
		if self, err = uuid.Parse(q.Device); err != nil {
			return submission.Plan{}, fmt.Errorf("%w: device must be a device id", ErrInvalid)
		}
	}
	if q.Submission != "" {
		return s.submissionPlan(ctx, user, q, project, self)
	}
	devs, err := s.st.SubmissionPlanDevices(ctx, db.SubmissionPlanDevicesParams{
		UserID: user, Project: project, FromTime: rg.From, ToTime: rangeEnd(rg),
	})
	if err != nil {
		return submission.Plan{}, fmt.Errorf("reading plan devices: %w", err)
	}
	if err := checkSealed(rg, devs); err != nil {
		return submission.Plan{}, err
	}
	rows, err := s.st.SubmissionPlanRows(ctx, db.SubmissionPlanRowsParams{
		UserID: user, Project: project, FromTime: rg.From, ToTime: rangeEnd(rg),
	})
	if err != nil {
		return submission.Plan{}, fmt.Errorf("reading plan rows: %w", err)
	}
	p := submission.Plan{
		Project: project, From: q.From, To: q.To,
		Devices: make([]submission.PlanDevice, 0, len(devs)),
		Minutes: make([]submission.Minute, 0, len(rows)),
	}
	for _, d := range devs {
		p.Devices = append(p.Devices, planDevice(d.ID, d.Name, d.SealedThrough, d.Revoked, self))
	}
	for _, r := range rows {
		p.Minutes = append(p.Minutes, minute(project, r.DeviceID, r.Minute, r.Label, r.Credited, r.Cuts, r.Flags,
			r.RulesVersion, r.ComputedAt, r.Snapshot.Int64, r.Snapshot.Valid))
	}
	return p, nil
}

// submissionPlan is the plan of one of the user's submissions: its frozen
// rows and the devices that have them. A part built from it lists only
// frozen rows, so AddPart takes it. The project and range must be the
// submission's (ErrInvalid otherwise, saying which they are), and the
// submission must still take parts (addable).
func (s *Service) submissionPlan(ctx context.Context, user uuid.UUID, q PlanQuery, project string, self uuid.UUID,
) (submission.Plan, error) {
	id, err := uuid.Parse(q.Submission)
	if err != nil {
		return submission.Plan{}, ErrNotFound
	}
	sub, err := s.st.SubmissionOfUser(ctx, db.SubmissionOfUserParams{ID: id, UserID: user})
	if err != nil {
		return submission.Plan{}, notFound(err, ErrNotFound, "reading submission")
	}
	if from, to := day(sub.FromDay), day(sub.ToDay); sub.Project != project || from != q.From || to != q.To {
		return submission.Plan{}, fmt.Errorf("%w: submission %s is %q from %s to %s; give that project and range",
			ErrInvalid, sub.ID, sub.Project, from, to)
	}
	deleted, err := s.st.SubmissionRawDeleted(ctx, sub.ID)
	if err != nil {
		return submission.Plan{}, fmt.Errorf("reading parts: %w", err)
	}
	if err := addable(sub, deleted); err != nil {
		return submission.Plan{}, err
	}
	devs, err := s.st.SubmissionFrozenDevices(ctx, sub.ID)
	if err != nil {
		return submission.Plan{}, fmt.Errorf("reading frozen devices: %w", err)
	}
	rows, err := s.st.SubmissionFrozenMinutes(ctx, sub.ID)
	if err != nil {
		return submission.Plan{}, fmt.Errorf("reading frozen minutes: %w", err)
	}
	p := submission.Plan{
		Project: project, From: q.From, To: q.To,
		Devices: make([]submission.PlanDevice, 0, len(devs)),
		Minutes: make([]submission.Minute, 0, len(rows)),
	}
	for _, d := range devs {
		p.Devices = append(p.Devices, planDevice(d.ID, d.Name, d.SealedThrough, d.Revoked, self))
	}
	for _, r := range rows {
		p.Minutes = append(p.Minutes, minute(project, r.DeviceID, r.Minute, r.Label, r.Credited, r.Cuts, r.Flags,
			r.RulesVersion, r.ComputedAt, r.Snapshot.Int64, r.Snapshot.Valid))
	}
	return p, nil
}

func planDevice(id uuid.UUID, name string, sealed pgtype.Date, revoked bool, self uuid.UUID) submission.PlanDevice {
	pd := submission.PlanDevice{ID: id.String(), Name: name, ThisDevice: id == self, Revoked: revoked}
	if sealed.Valid {
		d := day(sealed)
		pd.SealedThrough = &d
	}
	return pd
}

// checkSealed refuses a range ending after the last day every unrevoked
// device with rows in it has sealed (ADR 0020): their evidence for later
// days doesn't exist yet. A revoked device can't seal any more days, so it
// never holds a range back; its rows on days it didn't seal stay unbacked.
// The message says so, since a device that was replaced (a reinstall, a
// lost laptop) seals nothing more until it is revoked.
func checkSealed(rg submission.Range, devs []db.SubmissionPlanDevicesRow) error {
	const revokeHint = "; if that device is no longer in use (lost, replaced or reinstalled), revoke it on your " +
		"Logbook dashboard: a revoked device doesn't hold the range back, and its minutes there stay unbacked"
	live := slices.DeleteFunc(slices.Clone(devs), func(d db.SubmissionPlanDevicesRow) bool { return d.Revoked })
	for _, d := range live {
		if !d.SealedThrough.Valid {
			return fmt.Errorf("%w: device %q has sealed no evidence yet, so the range can't end yet%s", ErrInvalid, d.Name, revokeHint)
		}
	}
	for _, d := range live {
		if sealed := d.SealedThrough.Time.UTC(); rg.To.After(sealed) {
			return fmt.Errorf("%w: device %q has sealed evidence through %s, so the range can end on %s at the latest%s",
				ErrInvalid, d.Name, sealed.Format(time.DateOnly), lastSealed(live).Format(time.DateOnly), revokeHint)
		}
	}
	return nil
}

// lastSealed is the last day every device has sealed.
func lastSealed(devs []db.SubmissionPlanDevicesRow) time.Time {
	var out time.Time
	for i, d := range devs {
		if t := d.SealedThrough.Time.UTC(); i == 0 || t.Before(out) {
			out = t
		}
	}
	return out
}

// minute builds a submission.Minute from a stored row.
func minute(project string, device uuid.UUID, at time.Time, label string, credited int16, cuts, flags []string,
	rules string, computedAt *time.Time, snapshot int64, hasSnapshot bool,
) submission.Minute {
	m := submission.Minute{
		DeviceID: device.String(), Minute: at.UTC(), Project: project, Label: label, Credited: int(credited),
		Cuts: nonNil(cuts), Flags: nonNil(flags), RulesVersion: rules,
	}
	if computedAt != nil && hasSnapshot {
		t, h := computedAt.UTC(), snapshot
		m.ComputedAt, m.Snapshot = &t, &h
	}
	return m
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
