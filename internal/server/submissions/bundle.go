package submissions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/submission"
)

// ErrEvidenceDeleted means the submission's stored parts were deleted
// (withdrawal or retention), so no bundle can be built.
var ErrEvidenceDeleted = errors.New("the submission's evidence was deleted")

// Bundle builds a submission's verification bundle from what the server
// stored (docs/api/submissions.md "The verification bundle"): the frozen
// rows, capped seconds and cap inputs, the parts as stored, and every
// stored record with block fields and the public key of each device with
// a frozen row or a part. It is what the admin export (ADR 0022) serves;
// it checks no caller, so its callers must. ErrNotFound for an unknown
// submission, ErrEvidenceDeleted once its parts are gone.
func (c *Checker) Bundle(ctx context.Context, id uuid.UUID) (submission.Bundle, error) {
	sub, err := c.st.SubmissionGet(ctx, id)
	if err != nil {
		return submission.Bundle{}, notFound(err, ErrNotFound, "reading submission")
	}
	frozen, err := c.st.SubmissionFrozenMinutes(ctx, sub.ID)
	if err != nil {
		return submission.Bundle{}, fmt.Errorf("reading frozen minutes: %w", err)
	}
	b := submission.Bundle{
		Format: submission.BundleFormat,
		Submission: submission.BundleSubmission{
			ID: sub.ID.String(), Project: sub.Project, From: day(sub.FromDay), To: day(sub.ToDay), CreatedAt: sub.CreatedAt.UTC(),
		},
		Devices: []submission.BundleDevice{}, Parts: []submission.Part{},
		Minutes: make([]submission.Minute, 0, len(frozen)),
	}
	ids := map[uuid.UUID]bool{}
	for _, r := range frozen {
		b.Minutes = append(b.Minutes, minute(sub.Project, r.DeviceID, r.Minute, r.Label, r.Credited, r.Cuts, r.Flags,
			r.RulesVersion, r.ComputedAt, r.Snapshot.Int64, r.Snapshot.Valid))
		ids[r.DeviceID] = true
	}
	if b.Capped, b.CapInputs, err = c.cap(ctx, sub.ID); err != nil {
		return submission.Bundle{}, err
	}
	parts, failed, err := c.parts(ctx, sub.ID)
	if err != nil {
		return submission.Bundle{}, err
	}
	if failed != "" {
		return submission.Bundle{}, ErrEvidenceDeleted
	}
	for id := range parts {
		ids[id] = true
	}
	devs, err := c.st.DeviceList(ctx, sub.UserID)
	if err != nil {
		return submission.Bundle{}, fmt.Errorf("reading devices: %w", err)
	}
	sorted := slices.SortedFunc(func(yield func(uuid.UUID) bool) {
		for id := range ids {
			if !yield(id) {
				return
			}
		}
	}, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for _, id := range sorted {
		i := slices.IndexFunc(devs, func(d db.Device) bool { return d.ID == id })
		if i < 0 {
			return submission.Bundle{}, fmt.Errorf("device %s of submission %s is gone", id, sub.ID)
		}
		records, _, err := c.records(ctx, sub.UserID, id)
		if err != nil {
			return submission.Bundle{}, err
		}
		if records == nil {
			records = []submission.EvidenceRecord{}
		}
		b.Devices = append(b.Devices, submission.BundleDevice{
			ID: id.String(), Name: devs[i].Name, PublicKey: devs[i].PublicKey, EvidenceDays: records,
		})
		if p, ok := parts[id]; ok {
			b.Parts = append(b.Parts, p)
		}
	}
	return b, nil
}
