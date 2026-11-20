package hackatime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// defaultImportFrom is the earliest day imported. Hackatime's /me doesn't
// expose an account creation date, and Logbook's heartbeat partitions (and
// Hackatime's current incarnation) start in 2024.
func defaultImportFrom() time.Time { return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) }

// Errors the HTTP layer maps to statuses.
var (
	// ErrAlreadyLinked means the Hackatime account is linked to another Logbook user.
	ErrAlreadyLinked = errors.New("hackatime: account already linked to another user")
	// ErrNotLinked means the user has no Hackatime link.
	ErrNotLinked = errors.New("hackatime: not linked")
	// ErrBanned means the linked Hackatime account is convicted (red), so the
	// link can't be removed or swapped for another account. Otherwise
	// unlinking, or relinking a clean account, would shed the ban.
	ErrBanned = errors.New("hackatime: linked account is banned")
)

// Enqueuer inserts River jobs inside the caller's transaction.
// *river.Client[pgx.Tx] implements it.
type Enqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// ServiceOptions are optional Service settings.
type ServiceOptions struct {
	Logger     *slog.Logger
	ImportFrom time.Time        // default 2024-01-01
	Now        func() time.Time // default time.Now
}

// Service owns the link between a Logbook user and their Hackatime account.
type Service struct {
	st         *store.Store
	box        *secretbox.Box
	client     *Client
	enq        Enqueuer
	log        *slog.Logger
	importFrom time.Time
	now        func() time.Time
}

// NewService builds a Service.
func NewService(st *store.Store, box *secretbox.Box, client *Client, enq Enqueuer, opts ServiceOptions) *Service {
	s := &Service{st: st, box: box, client: client, enq: enq, log: opts.Logger, importFrom: opts.ImportFrom, now: opts.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.importFrom.IsZero() {
		s.importFrom = defaultImportFrom()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// BeginLink starts the OAuth flow for userID. It returns the Hackatime
// authorize URL and the sealed state to keep in a cookie until the callback.
func (s *Service) BeginLink(userID uuid.UUID) (authURL, stateCookie string, err error) {
	if !s.client.OAuthConfigured() {
		return "", "", ErrNotConfigured
	}
	st, cookie, err := newLinkState(s.box, userID, s.now())
	if err != nil {
		return "", "", err
	}
	authURL, err = s.client.AuthCodeURL(st.State, st.Verifier)
	if err != nil {
		return "", "", err
	}
	return authURL, cookie, nil
}

// CompleteLink finishes the OAuth flow: it checks state, exchanges the code
// (with the PKCE verifier), reads the Hackatime profile and trust level,
// stores the encrypted token and queues the history import.
//
// A token is never revoked when linking fails. Hackatime's Doorkeeper reuses
// access tokens (reuse_access_token), so the token returned here is usually
// the very token already stored for this Hackatime account, by this user (a
// renewal) or by another user (the 409 case). Revoking it would silently break
// that existing link.
func (s *Service) CompleteLink(ctx context.Context, userID uuid.UUID, stateCookie, state, code string) error {
	verifier, err := openLinkState(s.box, stateCookie, userID, state, s.now())
	if err != nil {
		return err
	}
	if code == "" || len(code) > 512 {
		return ErrAuthorizationRejected
	}
	token, err := s.client.Exchange(ctx, code, verifier)
	if err != nil {
		return err
	}
	me, err := s.client.Me(ctx, token)
	if err != nil {
		return err
	}
	replaced, err := s.storeLink(ctx, userID, token, me)
	if err != nil {
		return err
	}
	if replaced != "" {
		// The user switched Hackatime accounts. Nobody else can hold the old
		// account's token (one link per Hackatime account), so give it back.
		s.revoke(ctx, replaced)
	}
	s.log.InfoContext(ctx, "hackatime linked", "user_id", userID, "hackatime_user_id", me.ID)
	return nil
}

// storeLink saves the link and queues the import in one transaction. When it
// replaces a link to a different Hackatime account, it returns that link's
// token so the caller can revoke it after commit.
func (s *Service) storeLink(ctx context.Context, userID uuid.UUID, token string, me Me) (replaced string, err error) {
	enc, err := s.box.Seal([]byte(token), userID[:])
	if err != nil {
		return "", fmt.Errorf("encrypting hackatime token: %w", err)
	}
	err = pgx.BeginFunc(ctx, s.st.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		owner, err := q.GetHackatimeLinkOwner(ctx, me.ID)
		switch {
		case err == nil && owner != userID:
			return ErrAlreadyLinked
		case err != nil && !store.IsNotFound(err):
			return fmt.Errorf("checking hackatime link owner: %w", err)
		}
		prev, err := q.LockHackatimeLink(ctx, userID)
		switch {
		case err == nil && prev.HackatimeUserID != me.ID:
			if TrustLevel(prev.TrustLevel).Banned() {
				return ErrBanned
			}
			// Switching Hackatime accounts: restart the import from scratch.
			if err := q.DeleteHackatimeImport(ctx, userID); err != nil {
				return fmt.Errorf("resetting hackatime import: %w", err)
			}
			if old, err := s.box.Open(prev.AccessTokenEnc, userID[:]); err == nil {
				replaced = string(old)
			}
		case err != nil && !store.IsNotFound(err):
			return fmt.Errorf("reading hackatime link: %w", err)
		}
		if _, err := q.UpsertHackatimeLink(ctx, db.UpsertHackatimeLinkParams{
			UserID:            userID,
			HackatimeUserID:   me.ID,
			HackatimeUsername: me.GitHubUsername,
			AccessTokenEnc:    enc,
			TrustLevel:        string(me.Trust),
		}); err != nil {
			return fmt.Errorf("saving hackatime link: %w", err)
		}
		today := s.now()
		if _, err := q.StartHackatimeImport(ctx, db.StartHackatimeImportParams{
			UserID:     userID,
			FromDate:   pgDate(s.importFrom),
			ToDate:     pgDate(today),
			RecentFrom: pgDate(recentStart(s.importFrom, today)),
		}); err != nil {
			return fmt.Errorf("starting hackatime import: %w", err)
		}
		if err := audit(ctx, q, userID, "hackatime.linked", map[string]any{
			"hackatime_user_id": me.ID, "trust_level": me.Trust,
		}); err != nil {
			return err
		}
		if _, err := s.enq.InsertTx(ctx, tx, ImportArgs{UserID: userID}, nil); err != nil {
			return fmt.Errorf("queueing hackatime import: %w", err)
		}
		return nil
	})
	switch {
	case isUniqueViolation(err):
		return "", ErrAlreadyLinked // lost a race with another user linking the same account
	case err != nil:
		return "", err
	case replaced == token:
		return "", nil // never revoke the token we just stored
	}
	return replaced, nil
}

// Unlink removes the link and its import state, then revokes the token at
// Hackatime (best effort). Heartbeats already imported stay. A banned (red)
// link can't be removed, so the ban keeps applying to this Logbook account;
// the user can still revoke Logbook's access on Hackatime.
func (s *Service) Unlink(ctx context.Context, userID uuid.UUID) error {
	var link db.HackatimeLink
	err := pgx.BeginFunc(ctx, s.st.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		link, err = q.LockHackatimeLink(ctx, userID)
		if store.IsNotFound(err) {
			return ErrNotLinked
		}
		if err != nil {
			return fmt.Errorf("reading hackatime link: %w", err)
		}
		if TrustLevel(link.TrustLevel).Banned() {
			return ErrBanned
		}
		if _, err := q.DeleteHackatimeLink(ctx, userID); err != nil {
			return fmt.Errorf("deleting hackatime link: %w", err)
		}
		if err := q.DeleteHackatimeImport(ctx, userID); err != nil {
			return fmt.Errorf("deleting hackatime import: %w", err)
		}
		return audit(ctx, q, userID, "hackatime.unlinked", map[string]any{"hackatime_user_id": link.HackatimeUserID})
	})
	if err != nil {
		return err
	}
	if token, err := s.box.Open(link.AccessTokenEnc, userID[:]); err == nil {
		s.revoke(ctx, string(token))
	}
	s.log.InfoContext(ctx, "hackatime unlinked", "user_id", userID, "hackatime_user_id", link.HackatimeUserID)
	return nil
}

// revoke gives a token back to Hackatime, logging (without the token) on failure.
func (s *Service) revoke(ctx context.Context, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.client.Revoke(ctx, token); err != nil && !errors.Is(err, ErrNotConfigured) {
		s.log.WarnContext(ctx, "revoking hackatime token failed", "err", err)
	}
}

// Status is the user-facing view of a Hackatime link.
type Status struct {
	Linked bool
	Link   db.HackatimeLink // zero unless Linked
	Import *db.HackatimeImport
}

// Status returns userID's link and import progress.
func (s *Service) Status(ctx context.Context, userID uuid.UUID) (Status, error) {
	link, err := s.st.GetHackatimeLink(ctx, userID)
	if store.IsNotFound(err) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("reading hackatime link: %w", err)
	}
	st := Status{Linked: true, Link: link}
	imp, err := s.st.GetHackatimeImport(ctx, userID)
	switch {
	case err == nil:
		st.Import = &imp
	case !store.IsNotFound(err):
		return Status{}, fmt.Errorf("reading hackatime import: %w", err)
	}
	return st, nil
}

// TrustLevel is the user's last known Hackatime trust level, unmasked, for
// gating (TrustLevel.Banned blocks submissions). TrustUnknown means Logbook
// can't currently verify the user (for example they revoked Logbook's access
// on Hackatime), and gating should treat it as unverified, not as clean. A
// red link can't be unlinked or swapped, so a ban can't be shed that way. It
// returns ErrNotLinked when the user has no link; the caller decides what
// that means.
func (s *Service) TrustLevel(ctx context.Context, userID uuid.UUID) (TrustLevel, error) {
	link, err := s.st.GetHackatimeLink(ctx, userID)
	if store.IsNotFound(err) {
		return TrustUnknown, ErrNotLinked
	}
	if err != nil {
		return TrustUnknown, fmt.Errorf("reading hackatime trust level: %w", err)
	}
	return TrustLevel(link.TrustLevel), nil
}

func audit(ctx context.Context, q *db.Queries, userID uuid.UUID, action string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorUserID: pgtype.UUID{Bytes: userID, Valid: true},
		Action:      action,
		SubjectType: "hackatime_link",
		SubjectID:   userID.String(),
		Data:        b,
	}); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return nil
}

func pgDate(t time.Time) pgtype.Date {
	t = t.UTC()
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}
