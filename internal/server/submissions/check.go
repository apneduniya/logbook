package submissions

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/hackatime"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/submission"
)

// Evidence reads a device's stored evidence chain; *ledger.Service
// implements it.
type Evidence interface {
	Days(ctx context.Context, userID, deviceID uuid.UUID, first, last time.Time) ([]ledger.StoredDay, error)
}

// Every stored record of a device lies between these days.
var (
	firstDay = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	lastDay  = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
)

// CheckerOptions configures a Checker.
type CheckerOptions struct {
	// DecodeSlots bounds how many parts are decoded at once server-wide:
	// pass the Service's (Options.DecodeSlots), so uploads and re-checks
	// share them. nil gets its own DefaultDecodeSlots, shared with nothing.
	DecodeSlots *DecodeSlots
	Logger      *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Checker runs the re-check job and the raw-row retention job.
type Checker struct {
	st    *store.Store
	ev    Evidence
	slots *DecodeSlots
	log   *slog.Logger
	now   func() time.Time
	// decoded, when set, is told +1 when the re-check holds a decoded part
	// and -1 when it has dropped it (tests).
	decoded func(delta int)
}

// NewChecker returns a Checker.
func NewChecker(st *store.Store, ev Evidence, opts CheckerOptions) *Checker {
	c := &Checker{st: st, ev: ev, slots: opts.DecodeSlots, log: opts.Logger}
	if c.slots == nil {
		c.slots = NewDecodeSlots(0)
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c.now = func() time.Time { return now().UTC() }
	return c
}

// outcome is what one re-check found: a failure reason, or every frozen
// row's result and the packet.
type outcome struct {
	failed  string
	results []submission.RowResult
	packet  submission.Packet
}

// Verify re-checks a submission that is verifying (docs/api/submissions.md
// "The re-check job"): each stored part with submission.VerifyPart against
// every stored evidence record of its device and the device's public key;
// rows of devices with no part are unbacked; then the cap and the packet.
// Parts are read, decoded and verified one at a time, each within one of
// the shared decode slots (waiting for one as long as the job runs), and
// dropped before the next is read.
// A malformed part, a cap that doesn't recompute or a rules version this
// server can't run sets the submission failed; anything else is returned
// for River to retry. The result is written only if no part was added
// meanwhile, so re-running it is safe and gives the same packet.
func (c *Checker) Verify(ctx context.Context, id uuid.UUID) error {
	sub, err := c.st.SubmissionGet(ctx, id)
	if store.IsNotFound(err) {
		return nil // the account was deleted
	}
	if err != nil {
		return fmt.Errorf("reading submission: %w", err)
	}
	if sub.Status != StatusVerifying {
		return nil
	}
	out, err := c.check(ctx, sub)
	if err != nil {
		return err
	}
	return c.save(ctx, sub, out)
}

func (c *Checker) check(ctx context.Context, sub db.Submission) (outcome, error) {
	frozen, err := c.st.SubmissionFrozenMinutes(ctx, sub.ID)
	if err != nil {
		return outcome{}, fmt.Errorf("reading frozen minutes: %w", err)
	}
	rows := map[uuid.UUID][]submission.Minute{}
	all := make([]submission.Minute, 0, len(frozen))
	for _, r := range frozen {
		m := minute(sub.Project, r.DeviceID, r.Minute, r.Label, r.Credited, r.Cuts, r.Flags, r.RulesVersion,
			r.ComputedAt, r.Snapshot.Int64, r.Snapshot.Valid)
		rows[r.DeviceID] = append(rows[r.DeviceID], m)
		all = append(all, m)
	}
	capped, inputs, err := c.cap(ctx, sub.ID)
	if err != nil {
		return outcome{}, err
	}
	pr, err := c.st.SubmissionPartDevices(ctx, sub.ID)
	if err != nil {
		return outcome{}, fmt.Errorf("reading parts: %w", err)
	}
	hasPart := make(map[uuid.UUID]bool, len(pr))
	for _, r := range pr {
		if r.Deleted {
			return c.fail(ctx, sub.ID, failDeleted, "a part's raw rows were deleted"), nil
		}
		hasPart[r.DeviceID] = true
	}
	devs, err := c.st.DeviceList(ctx, sub.UserID)
	if err != nil {
		return outcome{}, fmt.Errorf("reading devices: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(rows)+len(hasPart))
	for id := range rows {
		ids = append(ids, id)
	}
	for id := range hasPart {
		if _, ok := rows[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })

	in := submission.PacketInput{
		Project: sub.Project, Range: subRange(sub), Capped: capped, CapInputs: inputs,
		Trust: submission.Trust{
			AtSubmission:    maskTrust(hackatime.TrustLevel(sub.TrustAtSubmission.String), sub.TrustAtSubmission.Valid),
			HackatimeLinked: sub.TrustAtSubmission.Valid,
		},
	}
	for _, id := range ids {
		i := slices.IndexFunc(devs, func(d db.Device) bool { return d.ID == id })
		if i < 0 {
			return outcome{}, fmt.Errorf("device %s of submission %s is gone", id, sub.ID)
		}
		dev := devs[i]
		records, sealed, err := c.records(ctx, sub.UserID, id)
		if err != nil {
			return outcome{}, err
		}
		if len(rows[id]) > 0 {
			in.Devices = append(in.Devices, submission.DeviceInfo{ID: id.String(), Name: dev.Name, OS: dev.Os, Sealed: sealed})
		}
		if !hasPart[id] {
			in.Results = append(in.Results, submission.Unbacked(rows[id], submission.ReasonNoPart)...)
			continue
		}
		checked, failed, err := c.verifyPart(ctx, sub.ID, id,
			submission.Device{ID: id.String(), PublicKey: ed25519.PublicKey(dev.PublicKey), Records: records}, rows[id])
		if err != nil {
			return outcome{}, err
		}
		if failed == evidenceDeleted {
			return c.fail(ctx, sub.ID, failDeleted, "a part's raw rows were deleted", "device", id), nil
		}
		if failed != "" {
			return c.fail(ctx, sub.ID, failPart(dev.Name), failed, "device", id), nil
		}
		in.Results = append(in.Results, checked.Results...)
		in.Revealed = append(in.Revealed, submission.DeviceRows{DeviceID: id.String(), Rows: checked.Revealed})
	}
	if err := submission.CheckCap(sub.Project, all, capped, inputs); err != nil {
		if errors.Is(err, submission.ErrMalformed) {
			return c.fail(ctx, sub.ID, failCounts, reason(err)), nil
		}
		return outcome{}, err
	}
	if in.Raw, err = c.raw(ctx, sub); err != nil {
		return outcome{}, err
	}
	settings, err := c.st.ProjectsSettingsGet(ctx, db.ProjectsSettingsGetParams{UserID: sub.UserID, Name: sub.Project})
	switch {
	case err == nil:
		in.RepoURL = settings.RepoUrl
	case !store.IsNotFound(err):
		return outcome{}, fmt.Errorf("reading project settings: %w", err)
	}
	p, err := submission.BuildPacket(in)
	if errors.Is(err, submission.ErrMalformed) {
		return c.fail(ctx, sub.ID, failCounts, reason(err)), nil
	}
	if err != nil {
		return outcome{}, fmt.Errorf("building packet: %w", err)
	}
	return outcome{results: in.Results, packet: p}, nil
}

// The failure reasons the submitter reads (docs/api/submissions.md,
// "The re-check job"): one short plain sentence each. What exactly failed,
// with its hashes, minutes and keys, goes to the log instead.
const (
	failDeleted = "The evidence sent with it was deleted before it could be checked."
	failCounts  = "The counted time sent with it doesn't add up when Logbook recounts it."
)

// failPart is the reason when a device's part doesn't verify.
func failPart(device string) string {
	return fmt.Sprintf("The evidence from %q doesn't match its signed records.", device)
}

// fail is the outcome of a submission that failed its check: the plain
// reason is stored, and the detail is logged.
func (c *Checker) fail(ctx context.Context, id uuid.UUID, plain, detail string, attrs ...any) outcome {
	c.log.InfoContext(ctx, "submission failed its check",
		append([]any{"submission", id, "reason", plain, "detail", detail}, attrs...)...)
	return outcome{failed: plain}
}

// reason is a malformed-submission error without its generic prefix.
func reason(err error) string {
	return strings.TrimPrefix(err.Error(), submission.ErrMalformed.Error()+": ")
}

// cap reads the frozen capped seconds and cap inputs.
func (c *Checker) cap(ctx context.Context, id uuid.UUID) ([]submission.CappedMinute, []submission.CapInput, error) {
	cr, err := c.st.SubmissionCapped(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("reading capped seconds: %w", err)
	}
	capped := make([]submission.CappedMinute, len(cr))
	for i, r := range cr {
		capped[i] = submission.CappedMinute{Minute: r.Minute.UTC(), Seconds: r.Seconds}
	}
	ir, err := c.st.SubmissionCapInputs(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("reading cap inputs: %w", err)
	}
	inputs := make([]submission.CapInput, len(ir))
	for i, r := range ir {
		inputs[i] = submission.CapInput{DeviceID: r.DeviceID.String(), Minute: r.Minute.UTC(), Project: r.Project, Credited: int(r.Credited)}
	}
	return capped, inputs, nil
}

// evidenceDeleted is what verifyPart reports once a part's raw rows were
// deleted, so the submission can't be checked any more (failDeleted).
const evidenceDeleted = "the evidence was deleted"

// verifyPart reads one device's stored part, decodes it and verifies it,
// all within a decode slot, waiting for one until ctx ends. The decoded
// part is dropped when it returns; only its results and revealed rows
// stay. failed is why the submission fails (a malformed part, or its raw
// rows deleted meanwhile); err is for River to retry.
func (c *Checker) verifyPart(ctx context.Context, id, device uuid.UUID, dev submission.Device, frozen []submission.Minute) (
	checked submission.Checked, failed string, err error,
) {
	release, err := c.slots.acquire(ctx)
	if err != nil {
		return submission.Checked{}, "", fmt.Errorf("waiting for a decode slot: %w", err)
	}
	defer release()
	data, err := c.st.SubmissionPartData(ctx, db.SubmissionPartDataParams{SubmissionID: id, DeviceID: device})
	if store.IsNotFound(err) || (err == nil && data == nil) {
		return submission.Checked{}, evidenceDeleted, nil // withdrawn or expired since it was listed
	}
	if err != nil {
		return submission.Checked{}, "", fmt.Errorf("reading the part of device %s: %w", device, err)
	}
	part, err := decodePart(data)
	if err != nil {
		return submission.Checked{}, "", fmt.Errorf("part of device %s: %w", device, err)
	}
	if c.decoded != nil {
		c.decoded(1)
		defer c.decoded(-1)
	}
	checked, err = submission.VerifyPart(part, dev, frozen)
	if errors.Is(err, submission.ErrMalformed) {
		return submission.Checked{}, reason(err), nil
	}
	if err != nil {
		return submission.Checked{}, "", fmt.Errorf("verifying the part of device %s: %w", device, err)
	}
	return checked, "", nil
}

// records reads a device's every stored record with block fields, for the
// verifier, and the days of every record, for the packet's gaps.
func (c *Checker) records(ctx context.Context, user, device uuid.UUID) ([]submission.EvidenceRecord, []string, error) {
	days, err := c.ev.Days(ctx, user, device, firstDay, lastDay)
	if err != nil {
		return nil, nil, fmt.Errorf("reading evidence of device %s: %w", device, err)
	}
	var records []submission.EvidenceRecord
	sealed := make([]string, 0, len(days))
	for _, d := range days {
		sealed = append(sealed, d.Record.Day)
		if !d.HasBlock {
			continue
		}
		r := d.Record
		records = append(records, submission.EvidenceRecord{
			Day: r.Day, PrevChainHash: r.PrevChainHash, ContentHash: r.ContentHash, ChainHash: d.ChainHash,
			Signature: d.Signature, RulesVersion: r.RulesVersion, EventCounts: d.EventCounts,
			SeqFrom: r.SeqFrom, SeqTo: r.SeqTo, Buckets: r.Buckets, TimeFrom: r.TimeFrom, TimeTo: r.TimeTo,
		})
	}
	return records, sealed, nil
}

// raw reads time at the keyboard for the project, per day.
func (c *Checker) raw(ctx context.Context, sub db.Submission) ([]submission.RawDay, error) {
	rg := subRange(sub)
	rr, err := c.st.SubmissionRawDays(ctx, db.SubmissionRawDaysParams{
		UserID: sub.UserID, Project: sub.Project, FromTime: rg.From, ToTime: rangeEnd(rg),
	})
	if err != nil {
		return nil, fmt.Errorf("reading raw time: %w", err)
	}
	out := make([]submission.RawDay, len(rr))
	for i, r := range rr {
		out[i] = submission.RawDay{Day: r.Day, Seconds: r.Seconds, AISeconds: r.AiSeconds}
	}
	return out, nil
}

// resultRow is one frozen row's result as SubmissionSetResults reads it.
type resultRow struct {
	DeviceID       string    `json:"device_id"`
	Minute         time.Time `json:"minute"`
	Result         string    `json:"result"`
	Reason         string    `json:"reason"`
	ServerLabel    *string   `json:"server_label"`
	ServerCredited *int      `json:"server_credited"`
	ServerCuts     []string  `json:"server_cuts"`
}

// save writes an outcome, unless the submission stopped verifying or got
// another part since it was read.
func (c *Checker) save(ctx context.Context, sub db.Submission, out outcome) error {
	var results []byte
	if out.failed == "" {
		rows := make([]resultRow, len(out.results))
		for i, r := range out.results {
			row := resultRow{DeviceID: r.Row.DeviceID, Minute: r.Row.Minute.UTC(), Result: string(r.Result), Reason: r.Reason}
			if r.Result == submission.ResultMismatch && r.Server != nil {
				label, credited := r.Server.Label, r.Server.Credited
				row.ServerLabel, row.ServerCredited, row.ServerCuts = &label, &credited, nonNil(r.Server.Cuts)
			}
			rows[i] = row
		}
		var err error
		if results, err = json.Marshal(rows); err != nil {
			return fmt.Errorf("encoding results: %w", err)
		}
	}
	packet, err := json.Marshal(out.packet)
	if err != nil {
		return fmt.Errorf("encoding packet: %w", err)
	}
	return c.st.InTx(ctx, func(q *db.Queries) error {
		cur, err := q.SubmissionLock(ctx, sub.ID)
		if store.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking submission: %w", err)
		}
		if cur.Status != StatusVerifying || cur.PartsVersion != sub.PartsVersion {
			return nil // withdrawn, or a newer part's re-check is queued
		}
		now := c.now()
		if out.failed != "" {
			c.log.InfoContext(ctx, "submission failed its re-check", "submission_id", sub.ID)
			if err := q.SubmissionSetFailed(ctx, db.SubmissionSetFailedParams{ID: sub.ID, Error: out.failed, VerifiedAt: now}); err != nil {
				return fmt.Errorf("marking submission failed: %w", err)
			}
			return nil
		}
		if err := q.SubmissionSetResults(ctx, db.SubmissionSetResultsParams{SubmissionID: sub.ID, Rows: results}); err != nil {
			return fmt.Errorf("writing results: %w", err)
		}
		p := out.packet
		if err := q.SubmissionSetReady(ctx, db.SubmissionSetReadyParams{
			ID: sub.ID, Packet: packet, VerifiedAt: now,
			VerifiedMinutes: int32(p.Minutes.Verified), MismatchMinutes: int32(p.Minutes.Mismatch), //nolint:gosec // at most 92 days of minutes
			UnbackedMinutes: int32(p.Minutes.Unbacked), //nolint:gosec // as above
			CountedSeconds:  p.CountedSeconds, RawSeconds: p.RawSeconds,
		}); err != nil {
			return fmt.Errorf("marking submission ready: %w", err)
		}
		return nil
	})
}

// ExpireRaw deletes the stored parts of submissions whose latest decision,
// or creation when there is none, is more than a year old (ADR 0020
// "Retention"), and returns how many parts it deleted. It locks each such
// submission as a decision does, so a decision being recorded makes it
// wait and then keep that submission's parts.
func (c *Checker) ExpireRaw(ctx context.Context) (int64, error) {
	now := c.now()
	n, err := c.st.SubmissionExpireRaw(ctx, db.SubmissionExpireRawParams{
		DeletedAt: now, Cutoff: now.AddDate(-rawRetentionYears, 0, 0),
	})
	if err != nil {
		return 0, fmt.Errorf("deleting expired raw rows: %w", err)
	}
	if n > 0 {
		c.log.InfoContext(ctx, "deleted expired submission parts", "parts", n)
	}
	return n, nil
}
