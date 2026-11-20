package auth

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Sign-in methods recorded in the audit log.
const (
	methodHCA = "hca"
	methodDev = "dev"
)

// signedIn is the result of a successful sign-in: the user, their new
// session, and the raw session id for the cookie.
type signedIn struct {
	user   db.User
	sess   session
	cookie string
}

// signIn upserts the user for in, replaces the browser's previous session
// (oldSession, the hash from its cookie, or nil) with a new one so a session
// id can't be fixed before sign-in, and writes the auth.login audit row.
func (s *Service) signIn(ctx context.Context, in signInIdentity, method string, oldSession []byte) (signedIn, error) {
	u, err := s.upsertUser(ctx, in)
	if err != nil {
		return signedIn{}, err
	}
	out := signedIn{user: u}
	err = s.st.InTx(ctx, func(q *db.Queries) error {
		if oldSession != nil {
			if err := q.DeleteSession(ctx, oldSession); err != nil {
				return fmt.Errorf("deleting previous session: %w", err)
			}
		}
		var cerr error
		out.cookie, out.sess, cerr = s.createSession(ctx, q, u.ID)
		if cerr != nil {
			return cerr
		}
		return audit(ctx, q, u.ID, "auth.login", "user", u.ID.String(), map[string]any{"method": method})
	})
	if err != nil {
		return signedIn{}, err
	}
	return out, nil
}

// logout deletes one session and writes the auth.logout audit row.
func (s *Service) logout(ctx context.Context, userID uuid.UUID, sessionHash []byte) error {
	return s.st.InTx(ctx, func(q *db.Queries) error {
		if err := q.DeleteSession(ctx, sessionHash); err != nil {
			return fmt.Errorf("deleting session: %w", err)
		}
		return audit(ctx, q, userID, "auth.logout", "user", userID.String(), nil)
	})
}

// currentUser loads the full account row for a signed-in user.
func (s *Service) currentUser(ctx context.Context, userID uuid.UUID) (db.User, error) {
	u, err := s.st.GetUserByID(ctx, userID)
	if err != nil {
		return db.User{}, fmt.Errorf("loading session user: %w", err)
	}
	return u, nil
}
