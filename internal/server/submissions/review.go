package submissions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/submission"
)

// Decision values.
const (
	DecisionApproved = "approved"
	DecisionAdjusted = "adjusted"
	DecisionRejected = "rejected"
)

// MaxReasonLen bounds a decision's reason, in characters.
const MaxReasonLen = 500

// ReviewSubmission is the submission as the review link shows it.
type ReviewSubmission struct {
	ID        uuid.UUID `json:"id"`
	Project   string    `json:"project"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Status    string    `json:"status"`
	User      Submitter `json:"user"`
	Program   *Program  `json:"program"`
	CreatedAt time.Time `json:"created_at"`
	// Version identifies the packet: it changes whenever a part is added
	// (and the submission is checked again). A decision may echo it.
	Version int32 `json:"version"`
}

// Review is GET /api/v1/review/{token}. Packet is nil unless the
// submission is ready.
type Review struct {
	Submission ReviewSubmission   `json:"submission"`
	Packet     *submission.Packet `json:"packet"`
	Decision   *Decision          `json:"decision"`
}

// byToken reads the submission a review token names:
// ErrNotFound for an unknown or malformed token.
func (s *Service) byToken(ctx context.Context, token string) (db.Submission, error) {
	if !wellFormedToken(token) {
		return db.Submission{}, ErrNotFound
	}
	sub, err := s.st.SubmissionByTokenHash(ctx, tokenHash(token))
	if err != nil {
		return db.Submission{}, notFound(err, ErrNotFound, "reading submission")
	}
	return sub, nil
}

// Review returns what the review link shows (no sign-in: the token is the
// credential). It is ErrNotFound for an unknown token and ErrWithdrawn for
// a withdrawn submission. A failed submission shows its status only.
func (s *Service) Review(ctx context.Context, token string) (Review, error) {
	sub, err := s.byToken(ctx, token)
	if err != nil {
		return Review{}, err
	}
	if sub.Status == StatusWithdrawn {
		return Review{}, ErrWithdrawn
	}
	sums, err := s.summaries(ctx, []db.Submission{sub}, forProgram)
	if err != nil {
		return Review{}, err
	}
	sum := sums[0]
	r := Review{
		Submission: ReviewSubmission{
			ID: sub.ID, Project: sub.Project, From: sum.From, To: sum.To, Status: sub.Status,
			User: *sum.User, Program: sum.Program, CreatedAt: sub.CreatedAt.UTC(), Version: sub.PartsVersion,
		},
		Decision: sum.Decision,
	}
	r.Packet, err = s.servePacket(ctx, sub, s.link(token))
	return r, err
}

// DecisionInput is the body of POST /api/v1/review/{token}/decision.
type DecisionInput struct {
	Value           string  `json:"value"`
	ApprovedSeconds *int64  `json:"approved_seconds"`
	Reason          *string `json:"reason"`
	// Version is the review's submission.version the reviewer read, or
	// nil. When given, the decision is refused if the submission was
	// checked again since (a part was added).
	Version *int32 `json:"version"`
}

// check validates the input against the submission's counted seconds and
// returns the reason to store.
func (in DecisionInput) check(counted int64) (string, error) {
	reason := ""
	if in.Reason != nil {
		reason = strings.TrimSpace(*in.Reason)
	}
	switch in.Value {
	case DecisionApproved:
		if in.ApprovedSeconds != nil || reason != "" {
			return "", fmt.Errorf("%w: approved takes no approved_seconds or reason", ErrInvalid)
		}
		return "", nil
	case DecisionAdjusted:
		if in.ApprovedSeconds == nil || *in.ApprovedSeconds < 0 || *in.ApprovedSeconds >= counted {
			return "", fmt.Errorf("%w: adjusted needs approved_seconds from 0 to below the counted %d", ErrInvalid, counted)
		}
	case DecisionRejected:
		if in.ApprovedSeconds != nil {
			return "", fmt.Errorf("%w: rejected takes no approved_seconds", ErrInvalid)
		}
	default:
		return "", fmt.Errorf("%w: value must be approved, adjusted or rejected", ErrInvalid)
	}
	if n := utf8.RuneCountInString(reason); n < 1 || n > MaxReasonLen || !utf8.ValidString(reason) {
		return "", fmt.Errorf("%w: %s needs a reason of 1 to %d characters", ErrInvalid, in.Value, MaxReasonLen)
	}
	return reason, nil
}

// Decide records a program member's decision on the submission a review
// token names (ADR 0020 "Decisions"). The reviewer must be a member (owner
// or reviewer) of the submission's live program and not its submitter
// (ErrForbidden); the submission must be ready, and still at the version
// the reviewer read when the input gives one (ErrConflict). The
// submitter's trust level is refreshed first when it is more than 24 hours
// old, and approving or adjusting is ErrConflict while they are banned or,
// when they were linked at submission, while they aren't linked to that
// same Hackatime account now (unlinked, or switched to another account):
// the level read now isn't the level of the account they submitted under.
// Every decision is kept, the newest wins, and each is audited
// submission.decide. Membership is checked again in the transaction that
// records the decision, under the submission's lock and then the
// program's (lockMember), so a reviewer removed meanwhile can't decide;
// the submission's lock also makes the retention job wait for the new
// deadline (ExpireRaw).
func (s *Service) Decide(ctx context.Context, reviewer uuid.UUID, token string, in DecisionInput) (Decision, error) {
	sub, err := s.byToken(ctx, token)
	if err != nil {
		return Decision{}, err
	}
	if sub.Status == StatusWithdrawn {
		return Decision{}, ErrWithdrawn
	}
	if !sub.ProgramAppID.Valid {
		return Decision{}, fmt.Errorf("%w: this submission isn't addressed to a program, so it takes no decisions", ErrForbidden)
	}
	if sub.UserID == reviewer {
		return Decision{}, fmt.Errorf("%w: you can't decide on your own submission", ErrForbidden)
	}
	app := uuid.UUID(sub.ProgramAppID.Bytes)
	if _, err := s.members.GetApp(ctx, reviewer, app); err != nil {
		if errors.Is(err, oauth.ErrNotFound) {
			return Decision{}, fmt.Errorf("%w: only the program's owner and reviewers can decide", ErrForbidden)
		}
		return Decision{}, fmt.Errorf("checking membership: %w", err)
	}
	if err := current(sub, in.Version); err != nil {
		return Decision{}, err
	}
	reason, err := in.check(sub.CountedSeconds)
	if err != nil {
		return Decision{}, err
	}
	trust, err := s.freshTrust(ctx, sub.UserID)
	if err != nil {
		return Decision{}, err
	}
	if in.Value != DecisionRejected {
		switch {
		case trust.banned():
			return Decision{}, fmt.Errorf("%w: the submitter's Hackatime account is banned now, so the hours can only be rejected", ErrConflict)
		case switchedAccount(sub, trust) && !trust.Linked:
			return Decision{}, fmt.Errorf("%w: the submitter unlinked Hackatime after submitting, so their trust level "+
				"can't be checked; the hours can only be rejected until they link it again", ErrConflict)
		case switchedAccount(sub, trust):
			return Decision{}, fmt.Errorf("%w: the submitter linked another Hackatime account after submitting, so the "+
				"account they submitted under can't be checked; the hours can only be rejected until they link it again", ErrConflict)
		}
	}
	err = s.st.InTx(ctx, func(q *db.Queries) error {
		cur, err := q.SubmissionLock(ctx, sub.ID)
		if err != nil {
			return fmt.Errorf("locking submission: %w", err)
		}
		if cur.Status == StatusWithdrawn {
			return ErrWithdrawn
		}
		if err := lockMember(ctx, q, app, reviewer); err != nil {
			return err
		}
		v := sub.PartsVersion
		if in.Version != nil {
			v = *in.Version
		}
		if err := current(cur, &v); err != nil {
			return err
		}
		now := s.now()
		var approved pgtype.Int8
		if in.ApprovedSeconds != nil {
			approved = pgtype.Int8{Int64: *in.ApprovedSeconds, Valid: true}
		}
		if _, err := q.SubmissionDecisionInsert(ctx, db.SubmissionDecisionInsertParams{
			SubmissionID: sub.ID, Value: in.Value, ApprovedSeconds: approved, Reason: reason,
			DecidedBy: pgtype.UUID{Bytes: reviewer, Valid: true}, DecidedAt: now,
		}); err != nil {
			return fmt.Errorf("recording decision: %w", err)
		}
		if err := q.SubmissionSetDecided(ctx, db.SubmissionSetDecidedParams{ID: sub.ID, DecidedAt: now}); err != nil {
			return fmt.Errorf("recording decision time: %w", err)
		}
		return audit(ctx, q, reviewer, "submission.decide", sub.ID, map[string]any{
			"value": in.Value, "approved_seconds": in.ApprovedSeconds, "reason": reason,
			"counted_seconds": sub.CountedSeconds, "program_id": app, "submitter_trust": string(trust.Level),
			"submitter_hackatime_id": hackatimeID(trust),
		})
	})
	if err != nil {
		return Decision{}, err
	}
	ds, err := s.decisions(ctx, []uuid.UUID{sub.ID})
	if err != nil {
		return Decision{}, err
	}
	return *ds[sub.ID], nil
}

// lockMember is ErrForbidden unless reviewer is a member of the live
// program app. It locks the app FOR SHARE until the transaction ends, then
// reads the membership: removing a member or deleting the app takes FOR NO
// KEY UPDATE on the app, so neither can commit between this check and the
// decision the transaction records. Taken after the submission's lock.
func lockMember(ctx context.Context, q *db.Queries, app, reviewer uuid.UUID) error {
	forbidden := fmt.Errorf("%w: only the program's owner and reviewers can decide", ErrForbidden)
	if _, err := q.SubmissionProgramLock(ctx, app); err != nil {
		if store.IsNotFound(err) {
			return forbidden
		}
		return fmt.Errorf("locking program: %w", err)
	}
	if _, err := q.SubmissionProgramMember(ctx, db.SubmissionProgramMemberParams{AppID: app, UserID: reviewer}); err != nil {
		if store.IsNotFound(err) {
			return forbidden
		}
		return fmt.Errorf("checking membership: %w", err)
	}
	return nil
}

// hackatimeID is the linked Hackatime account for the audit log, nil when
// there is none.
func hackatimeID(t trustState) *int64 {
	if !t.Linked {
		return nil
	}
	return &t.HackatimeID
}

// current is ErrConflict unless sub is ready and, when version isn't nil,
// at that version: a part added since changes the packet the reviewer read.
func current(sub db.Submission, version *int32) error {
	switch {
	case sub.Status != StatusReady:
		return fmt.Errorf("%w: the submission is %s, not ready", ErrConflict, sub.Status)
	case version != nil && *version != sub.PartsVersion:
		return fmt.Errorf("%w: the submission was checked again since you read it (a part was added); reload it", ErrConflict)
	}
	return nil
}

// Withdraw withdraws one of the user's submissions (DELETE
// /api/v1/me/submissions/{id}): its review link answers 410 and its stored
// parts are deleted. It is ErrDecided once a decision exists; withdrawing
// again is a no-op. Audited submission.withdraw.
func (s *Service) Withdraw(ctx context.Context, user, id uuid.UUID) error {
	return s.st.InTx(ctx, func(q *db.Queries) error {
		sub, err := q.SubmissionLockOfUser(ctx, db.SubmissionLockOfUserParams{ID: id, UserID: user})
		if err != nil {
			return notFound(err, ErrNotFound, "reading submission")
		}
		if sub.Status == StatusWithdrawn {
			return nil
		}
		if sub.DecidedAt != nil {
			return ErrDecided
		}
		now := s.now()
		if err := q.SubmissionWithdraw(ctx, db.SubmissionWithdrawParams{ID: id, WithdrawnAt: now}); err != nil {
			return fmt.Errorf("withdrawing submission: %w", err)
		}
		n, err := q.SubmissionDeleteRaw(ctx, db.SubmissionDeleteRawParams{SubmissionID: id, DeletedAt: now})
		if err != nil {
			return fmt.Errorf("deleting raw rows: %w", err)
		}
		return audit(ctx, q, user, "submission.withdraw", id, map[string]any{
			"status": sub.Status, "parts_deleted": n,
		})
	})
}
