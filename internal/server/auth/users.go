package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

const (
	maxDisplayName = 100
	maxEmail       = 254
	// sequentialUsernameTries is how many of base, base2, ... we try before
	// switching to random suffixes.
	sequentialUsernameTries = 20
	randomUsernameTries     = 10
)

// signInIdentity is a verified identity ready to become (or match) a Logbook user.
type signInIdentity struct {
	hcaID    string
	username string // preferred base username, already sanitized
	// randomSuffix skips base, base2, ... and goes straight to a random
	// numeric suffix. Used for the generic fallback name, so it neither
	// reveals how many such accounts exist nor wastes tries on taken names.
	randomSuffix bool
	displayName  string
	email        string
}

func signInFromHCA(id hcaIdentity) signInIdentity {
	base := baseUsername(id)
	return signInIdentity{
		hcaID:        id.ID,
		username:     base,
		randomSuffix: base == fallbackUsername,
		displayName:  clip(strings.TrimSpace(id.FirstName+" "+id.LastName), maxDisplayName),
		email:        clip(strings.ToLower(strings.TrimSpace(id.PrimaryEmail)), maxEmail),
	}
}

// clip truncates s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// upsertUser returns the user for in.hcaID, creating it with a unique
// username on first sign-in and refreshing name and email afterwards. The
// username never changes after creation.
func (s *Service) upsertUser(ctx context.Context, in signInIdentity) (db.User, error) {
	u, err := s.st.GetUserByHCAID(ctx, in.hcaID)
	switch {
	case err == nil:
		if u.DisplayName == in.displayName && u.Email == in.email {
			return u, nil
		}
		u, err = s.st.UpdateSignInProfile(ctx, db.UpdateSignInProfileParams{
			ID: u.ID, DisplayName: in.displayName, Email: in.email,
		})
		if err != nil {
			return db.User{}, fmt.Errorf("updating user profile: %w", err)
		}
		return u, nil
	case !store.IsNotFound(err):
		return db.User{}, fmt.Errorf("looking up user by identity: %w", err)
	}

	first := 0
	if in.randomSuffix {
		first = sequentialUsernameTries
	}
	for i := first; i < sequentialUsernameTries+randomUsernameTries; i++ {
		name := usernameCandidate(in.username, i)
		if i >= sequentialUsernameTries {
			name = in.username + randomDigits(6)
		}
		if !usernameRe.MatchString(name) {
			continue
		}
		taken, err := s.st.UsernameExists(ctx, name)
		if err != nil {
			return db.User{}, fmt.Errorf("checking username: %w", err)
		}
		if taken {
			continue
		}
		u, err := s.st.InsertSignInUser(ctx, db.InsertSignInUserParams{
			HcaID: in.hcaID, Username: name, DisplayName: in.displayName, Email: in.email,
		})
		if isUsernameConflict(err) {
			continue // lost a race for this name; try the next one
		}
		if err != nil {
			return db.User{}, fmt.Errorf("creating user: %w", err)
		}
		return u, nil
	}
	return db.User{}, errors.New("creating user: no free username")
}

func isUsernameConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_username_key"
}

func randomDigits(n int) string {
	var b strings.Builder
	for range n {
		d, _ := rand.Int(rand.Reader, big.NewInt(10)) // crypto/rand never fails
		b.WriteString(d.String())
	}
	return b.String()
}
