package submissions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

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
// the range ends after a day some device with rows in it hasn't sealed.
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
		pd := submission.PlanDevice{ID: d.ID.String(), Name: d.Name, ThisDevice: d.ID == self}
		if d.SealedThrough.Valid {
			sealed := day(d.SealedThrough)
			pd.SealedThrough = &sealed
		}
		p.Devices = append(p.Devices, pd)
	}
	for _, r := range rows {
		p.Minutes = append(p.Minutes, minute(project, r.DeviceID, r.Minute, r.Label, r.Credited, r.Cuts, r.Flags,
			r.RulesVersion, r.ComputedAt, r.Snapshot.Int64, r.Snapshot.Valid))
	}
	return p, nil
}

// checkSealed refuses a range ending after the last day every device with
// rows in it has sealed (ADR 0020): their evidence for later days doesn't
// exist yet.
func checkSealed(rg submission.Range, devs []db.SubmissionPlanDevicesRow) error {
	for _, d := range devs {
		if !d.SealedThrough.Valid {
			return fmt.Errorf("%w: device %q has sealed no evidence yet, so the range can't end yet", ErrInvalid, d.Name)
		}
	}
	for _, d := range devs {
		if sealed := d.SealedThrough.Time.UTC(); rg.To.After(sealed) {
			return fmt.Errorf("%w: device %q has sealed evidence through %s, so the range can end on %s at the latest",
				ErrInvalid, d.Name, sealed.Format(time.DateOnly), lastSealed(devs).Format(time.DateOnly))
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
