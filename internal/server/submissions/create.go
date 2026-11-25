package submissions

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/submission"
)

// Statuses of a submission.
const (
	StatusVerifying = "verifying"
	StatusReady     = "ready"
	StatusFailed    = "failed"
	StatusWithdrawn = "withdrawn"
)

// Accepted is the answer to an upload: the re-check is queued.
type Accepted struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
	// ReviewURL is set when the submission is created.
	ReviewURL string `json:"review_url,omitempty"`
}

// Create makes a submission (POST /api/v1/submissions): it checks the
// upload's shape, its device and blocks, the program and the submitter's
// Hackatime trust; then, in one repeatable-read snapshot, it checks the
// range against what every device with rows has sealed and freezes the rows
// of every device, the capped seconds and the cap inputs; stores the part;
// and queues the re-check. Audited submission.create.
func (s *Service) Create(ctx context.Context, user uuid.UUID, up submission.Upload) (Accepted, error) {
	if err := up.Validate(); err != nil {
		return Accepted{}, invalid(err)
	}
	project, err := cleanProject(up.Project)
	if err != nil {
		return Accepted{}, err
	}
	rg, err := parseRange(up.From, up.To)
	if err != nil {
		return Accepted{}, err
	}
	device, err := s.checkPart(ctx, user, up.Part)
	if err != nil {
		return Accepted{}, err
	}
	var program pgtype.UUID
	if up.Program != "" {
		p, err := s.st.SubmissionPublicProgram(ctx, up.Program)
		if err != nil {
			return Accepted{}, notFound(err, ErrProgramNotFound, "reading program")
		}
		program = pgtype.UUID{Bytes: p.ID, Valid: true}
	}
	trust, err := s.freshTrust(ctx, user)
	if err != nil {
		return Accepted{}, err
	}
	if trust.banned() {
		return Accepted{}, ErrBanned
	}
	data, err := encodePart(up.Part)
	if err != nil {
		return Accepted{}, err
	}

	id, token := uuid.New(), newToken()
	enc, err := s.sealToken(id, token)
	if err != nil {
		return Accepted{}, err
	}
	var trustAt pgtype.Text
	if trust.Linked {
		trustAt = pgtype.Text{String: string(trust.Level), Valid: true}
	}
	now := s.now()
	err = pgx.BeginTxFunc(ctx, s.st.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		q := db.New(tx)
		devs, err := q.SubmissionPlanDevices(ctx, db.SubmissionPlanDevicesParams{
			UserID: user, Project: project, FromTime: rg.From, ToTime: rangeEnd(rg),
		})
		if err != nil {
			return fmt.Errorf("reading devices with rows: %w", err)
		}
		var credited int64
		for _, d := range devs {
			credited += d.CreditedRows
		}
		if credited == 0 {
			return ErrNothingToSubmit
		}
		if err := checkSealed(rg, devs); err != nil {
			return err
		}
		if err := q.SubmissionInsert(ctx, db.SubmissionInsertParams{
			ID: id, UserID: user, Project: project, FromDay: pgDate(rg.From), ToDay: pgDate(rg.To),
			ProgramAppID: program, TokenHash: tokenHash(token), TokenEnc: enc, TrustAtSubmission: trustAt, CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("inserting submission: %w", err)
		}
		if err := freeze(ctx, q, id); err != nil {
			return err
		}
		if _, err := q.SubmissionPartInsert(ctx, db.SubmissionPartInsertParams{
			SubmissionID: id, DeviceID: device, Data: data, Bytes: int32(len(data)), UploadedAt: now, //nolint:gosec // bounded by the upload limit
		}); err != nil {
			return fmt.Errorf("storing part: %w", err)
		}
		if err := audit(ctx, q, user, "submission.create", id, map[string]any{
			"project": project, "from": up.From, "to": up.To, "program": up.Program, "device_id": device,
			"trust_level": trustAt.String,
		}); err != nil {
			return err
		}
		return s.queue(ctx, tx, id)
	})
	if err != nil {
		return Accepted{}, err
	}
	return Accepted{ID: id, Status: StatusVerifying, ReviewURL: s.link(token)}, nil
}

// freeze copies the submission's rows, cap inputs and capped seconds from
// the transaction's snapshot.
func freeze(ctx context.Context, q *db.Queries, id uuid.UUID) error {
	if _, err := q.SubmissionFreezeMinutes(ctx, id); err != nil {
		return fmt.Errorf("freezing minutes: %w", err)
	}
	if _, err := q.SubmissionFreezeCapInputs(ctx, id); err != nil {
		return fmt.Errorf("freezing cap inputs: %w", err)
	}
	if _, err := q.SubmissionFreezeCapped(ctx, id); err != nil {
		return fmt.Errorf("freezing capped seconds: %w", err)
	}
	return nil
}

// AddPart adds another device's part to one of the user's submissions
// (POST /api/v1/submissions/{id}/parts) and queues the re-check of the
// whole submission. It is ErrConflict once a decision exists, after
// withdrawal or failure, or when the device already has a part.
func (s *Service) AddPart(ctx context.Context, user, id uuid.UUID, part submission.Part) (Accepted, error) {
	if err := part.Validate(); err != nil {
		return Accepted{}, invalid(err)
	}
	device, err := s.checkPart(ctx, user, part)
	if err != nil {
		return Accepted{}, err
	}
	trust, err := s.freshTrust(ctx, user)
	if err != nil {
		return Accepted{}, err
	}
	if trust.banned() {
		return Accepted{}, ErrBanned
	}
	data, err := encodePart(part)
	if err != nil {
		return Accepted{}, err
	}
	err = pgx.BeginFunc(ctx, s.st.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		sub, err := q.SubmissionLockOfUser(ctx, db.SubmissionLockOfUserParams{ID: id, UserID: user})
		if err != nil {
			return notFound(err, ErrNotFound, "reading submission")
		}
		switch {
		case sub.DecidedAt != nil:
			return fmt.Errorf("%w: a decision has been recorded; no parts can be added", ErrConflict)
		case sub.Status == StatusWithdrawn:
			return fmt.Errorf("%w: the submission was withdrawn", ErrConflict)
		case sub.Status == StatusFailed:
			return fmt.Errorf("%w: the submission failed its check; submit again", ErrConflict)
		}
		n, err := q.SubmissionPartInsert(ctx, db.SubmissionPartInsertParams{
			SubmissionID: id, DeviceID: device, Data: data, Bytes: int32(len(data)), UploadedAt: s.now(), //nolint:gosec // bounded by the upload limit
		})
		if err != nil {
			return fmt.Errorf("storing part: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: this device already has a part in the submission", ErrConflict)
		}
		if _, err := q.SubmissionPartsChanged(ctx, id); err != nil {
			return fmt.Errorf("marking submission for a re-check: %w", err)
		}
		return s.queue(ctx, tx, id)
	})
	if err != nil {
		return Accepted{}, err
	}
	return Accepted{ID: id, Status: StatusVerifying}, nil
}

// checkPart checks what a part names: the device is the user's, and every
// block is a day the device has a stored record with block fields for.
func (s *Service) checkPart(ctx context.Context, user uuid.UUID, part submission.Part) (uuid.UUID, error) {
	device, err := uuid.Parse(part.DeviceID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: device_id", ErrInvalid)
	}
	if _, err := s.st.SubmissionUserDevice(ctx, db.SubmissionUserDeviceParams{ID: device, UserID: user}); err != nil {
		if store.IsNotFound(err) {
			return uuid.Nil, fmt.Errorf("%w: unknown device", ErrInvalid)
		}
		return uuid.Nil, fmt.Errorf("reading device: %w", err)
	}
	if len(part.Blocks) == 0 {
		return device, nil
	}
	days := make([]pgtype.Date, len(part.Blocks))
	for i, b := range part.Blocks {
		t, err := time.ParseInLocation(time.DateOnly, b.Day, time.UTC)
		if err != nil {
			return uuid.Nil, fmt.Errorf("%w: block day %q", ErrInvalid, b.Day)
		}
		days[i] = pgDate(t)
	}
	stored, err := s.st.SubmissionBlockDays(ctx, db.SubmissionBlockDaysParams{DeviceID: device, Days: days})
	if err != nil {
		return uuid.Nil, fmt.Errorf("reading evidence days: %w", err)
	}
	have := make(map[string]bool, len(stored))
	for _, d := range stored {
		have[d] = true
	}
	for _, b := range part.Blocks {
		if !have[b.Day] {
			return uuid.Nil, fmt.Errorf("%w: the device has no stored evidence record for block %s", ErrInvalid, b.Day)
		}
	}
	return device, nil
}

// encodePart is a part as stored: its JSON, gzip-compressed.
func encodePart(p submission.Part) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(p); err != nil {
		return nil, fmt.Errorf("encoding part: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compressing part: %w", err)
	}
	if buf.Len() > math.MaxInt32 {
		return nil, fmt.Errorf("%w: the part is too large", ErrInvalid)
	}
	return buf.Bytes(), nil
}

// decodePart reads a stored part.
func decodePart(data []byte) (submission.Part, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return submission.Part{}, fmt.Errorf("decompressing part: %w", err)
	}
	var p submission.Part
	if err := json.NewDecoder(zr).Decode(&p); err != nil {
		return submission.Part{}, fmt.Errorf("decoding part: %w", err)
	}
	return p, nil
}

// queue queues the re-check of a submission in tx.
func (s *Service) queue(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	if _, err := s.enq.InsertTx(ctx, tx, VerifyArgs{SubmissionID: id}, nil); err != nil {
		return fmt.Errorf("queueing the re-check: %w", err)
	}
	return nil
}
