package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/submission"
)

// Errors the handlers map to HTTP statuses. ErrInvalid, ErrConflict and
// ErrForbidden wrap a message safe to show the caller.
var (
	// ErrNotFound: the submission doesn't exist or isn't the caller's (404).
	ErrNotFound = errors.New("submission not found")
	// ErrProgramNotFound: no live public program has the slug (404).
	ErrProgramNotFound = errors.New("program not found")
	// ErrBanned: the submitter's linked Hackatime account is banned (403).
	ErrBanned = errors.New("your Hackatime account is banned, so you can't submit hours")
	// ErrNothingToSubmit: no frozen row has credit (409).
	ErrNothingToSubmit = errors.New("no counted minutes in this range")
	// ErrDecided: a decision exists, so the submission can't be withdrawn (409).
	ErrDecided = errors.New("a decision has been recorded; it can't be withdrawn")
	// ErrWithdrawn: the review link of a withdrawn submission (410).
	ErrWithdrawn = errors.New("this submission was withdrawn")
	// ErrInvalid wraps a bad request (400).
	ErrInvalid = errors.New("invalid request")
	// ErrConflict wraps a request the submission's state refuses (409).
	ErrConflict = errors.New("conflict")
	// ErrForbidden wraps a caller who may not do this (403).
	ErrForbidden = errors.New("forbidden")
)

// rawRetentionYears is how long stored parts (the revealed raw rows) are kept
// after the latest decision, or after the submission when there is none.
const rawRetentionYears = 1

// Enqueuer queues River jobs inside a transaction; *river.Client[pgx.Tx]
// implements it.
type Enqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// TrustRefresher re-reads a user's Hackatime trust level and stores it;
// *hackatime.TrustRefresher implements it. It fails when Hackatime can't
// be reached, and the last known level then decides.
type TrustRefresher interface {
	Refresh(ctx context.Context, userID uuid.UUID) error
}

// Members resolves a user's membership of a live app; *oauth.Service
// implements it (oauth.ErrNotFound unless the user is a member).
type Members interface {
	GetApp(ctx context.Context, user, appID uuid.UUID) (oauth.App, error)
}

// Options configures a Service.
type Options struct {
	// ReviewBaseURL is the web UI's origin; a review link is
	// ReviewBaseURL + "/r/" + token. Required.
	ReviewBaseURL string
	// Enqueuer queues the re-check job. Required.
	Enqueuer Enqueuer
	// Members checks program membership. Required.
	Members Members
	// Trust refreshes trust levels older than hackatime.TrustMaxAge; nil
	// never refreshes (the stored level decides).
	Trust TrustRefresher
	// TrustTimeout bounds one refresh (default 10 s).
	TrustTimeout time.Duration
	// DecodeSlots bounds how many parts are decoded at once server-wide;
	// pass the same value to the Checker. nil gets its own
	// DefaultDecodeSlots, shared with nothing.
	DecodeSlots *DecodeSlots
	// MaxCompressedBytes and MaxDecompressedBytes bound an upload body
	// (default submission.MaxCompressedBytes and MaxDecompressedBytes).
	MaxCompressedBytes, MaxDecompressedBytes int64
	Logger                                   *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Service runs the HTTP-facing work: plans, uploads, reads, decisions and
// withdrawals. The re-check itself is Checker's.
type Service struct {
	st           *store.Store
	box          *secretbox.Box
	reviewBase   string
	enq          Enqueuer
	members      Members
	trust        TrustRefresher
	trustTimeout time.Duration
	slots        *DecodeSlots
	maxGzip      int64
	maxJSON      int64
	log          *slog.Logger
	now          func() time.Time
}

// NewService returns a Service backed by st. box encrypts review tokens.
func NewService(st *store.Store, box *secretbox.Box, opts Options) *Service {
	s := &Service{
		st: st, box: box, reviewBase: strings.TrimRight(opts.ReviewBaseURL, "/"),
		enq: opts.Enqueuer, members: opts.Members, trust: opts.Trust, trustTimeout: opts.TrustTimeout,
		slots: opts.DecodeSlots, maxGzip: opts.MaxCompressedBytes, maxJSON: opts.MaxDecompressedBytes, log: opts.Logger,
	}
	if s.slots == nil {
		s.slots = NewDecodeSlots(0)
	}
	if s.trustTimeout <= 0 {
		s.trustTimeout = 10 * time.Second
	}
	if s.maxGzip <= 0 {
		s.maxGzip = submission.MaxCompressedBytes
	}
	if s.maxJSON <= 0 {
		s.maxJSON = submission.MaxDecompressedBytes
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s.now = func() time.Time { return now().UTC() }
	return s
}

// invalid wraps a pkg/submission shape error as ErrInvalid, without its
// "malformed submission: " prefix.
func invalid(err error) error {
	msg := strings.TrimPrefix(err.Error(), submission.ErrMalformed.Error()+": ")
	return fmt.Errorf("%w: %s", ErrInvalid, msg)
}

// audit writes one audit_log row about a submission. data must never hold
// the review token.
func audit(ctx context.Context, q db.Querier, actor uuid.UUID, action string, id uuid.UUID, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorUserID: pgtype.UUID{Bytes: actor, Valid: actor != uuid.Nil},
		Action:      action, SubjectType: "submission", SubjectID: id.String(), Data: b,
	}); err != nil {
		return fmt.Errorf("writing audit log %s: %w", action, err)
	}
	return nil
}

// day turns a date column into YYYY-MM-DD.
func day(d pgtype.Date) string { return d.Time.Format(time.DateOnly) }

// pgDate is a UTC midnight as a date column.
func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: true} }

// subRange is a stored submission's range.
func subRange(sub db.Submission) submission.Range {
	return submission.Range{From: sub.FromDay.Time.UTC(), To: sub.ToDay.Time.UTC()}
}

// rangeEnd is the instant after a range's last day.
func rangeEnd(rg submission.Range) time.Time { return rg.To.AddDate(0, 0, 1) }

// rawDeletedAfter is the day a submission's stored parts are deleted.
func rawDeletedAfter(sub db.Submission) time.Time {
	from := sub.CreatedAt
	if sub.DecidedAt != nil {
		from = *sub.DecidedAt
	}
	return from.UTC().AddDate(rawRetentionYears, 0, 0)
}

// notFound maps a missing row to err.
func notFound(err, mapped error, what string) error {
	if store.IsNotFound(err) {
		return mapped
	}
	return fmt.Errorf("%s: %w", what, err)
}
