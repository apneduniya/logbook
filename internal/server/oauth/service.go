package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Lifetimes and limits.
const (
	// RequestTTL is how long a pending consent request lives.
	RequestTTL = 10 * time.Minute
	// CodeTTL is how long an authorization code can be exchanged.
	CodeTTL = 10 * time.Minute
	// AccessTokenTTL is the life of an access token issued for a user.
	AccessTokenTTL = 365 * 24 * time.Hour
	// RefreshTokenTTL is the life of a refresh token. Each refresh issues a
	// new one, so a program that refreshes at least this often never has
	// to ask the user again.
	RefreshTokenTTL = 2 * 365 * 24 * time.Hour
	// ClientTokenTTL is the life of a client-credentials token. The app can
	// always get another, so it is short.
	ClientTokenTTL = time.Hour

	// MaxAppsPerUser caps the live apps one user owns.
	MaxAppsPerUser = 10
	// MaxReviewers caps the reviewers of one app.
	MaxReviewers = 50
)

// Scopes (ADR 0021). profile and read are granted by users; submissions
// only to an app's own client-credentials token.
const (
	ScopeProfile     = "profile"
	ScopeRead        = "read"
	ScopeSubmissions = "submissions"
)

// Token prefixes, so a leaked value says what it is.
const (
	accessPrefix  = "lbo_"
	refreshPrefix = "lbr_"
	secretPrefix  = "lbs_"
	randomBytes   = 32
)

// Errors the JSON API maps to statuses.
var (
	// ErrInvalid wraps every validation failure; its message is safe to show.
	ErrInvalid = errors.New("invalid request")
	// ErrNotFound: no such live app, member, request or grant visible to the caller.
	ErrNotFound = errors.New("not found")
	// ErrNotOwner: the caller is a reviewer of the app, not its owner.
	ErrNotOwner = errors.New("only the app's owner can do this")
	// ErrSlugTaken: another app (live or deleted) has the slug.
	ErrSlugTaken = errors.New("slug is taken")
	// ErrTooManyApps: the user already owns MaxAppsPerUser live apps.
	ErrTooManyApps = errors.New("too many apps")
	// ErrAlreadyMember: the user is already a member of the app.
	ErrAlreadyMember = errors.New("already a member")
	// ErrTooManyReviewers: the app already has MaxReviewers reviewers.
	ErrTooManyReviewers = errors.New("too many reviewers")
	// ErrNoSecret: the app is a public client, which has no secret.
	ErrNoSecret = errors.New("public clients have no secret")
	// ErrUserNotFound: no user has the username.
	ErrUserNotFound = errors.New("user not found")
)

// Store is the database access the service needs. *store.Store satisfies it.
type Store interface {
	db.Querier
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
}

// Options configures a Service.
type Options struct {
	// WebURL is the web UI's origin (ADR 0011). The consent and error pages
	// live at WebURL + "/oauth/authorize". Empty means the UI shares the
	// API's origin.
	WebURL string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Service runs the OAuth provider.
type Service struct {
	st     Store
	webURL string
	now    func() time.Time
}

// NewService returns a Service backed by st.
func NewService(st Store, opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		st:     st,
		webURL: strings.TrimRight(opts.WebURL, "/"),
		now:    func() time.Time { return now().UTC() },
	}
}

// newSecret returns prefix plus 256 random bits, base64url without padding.
func newSecret(prefix string) string {
	b := make([]byte, randomBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// hash is the stored form of every secret value.
func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

func fromPG(id pgtype.UUID) uuid.UUID {
	if !id.Valid {
		return uuid.Nil
	}
	return id.Bytes
}

// audit writes one audit_log row about an app. actor may be uuid.Nil (an
// app acting on its own). data must never contain secrets.
func audit(ctx context.Context, q db.Querier, actor uuid.UUID, action string, appID uuid.UUID, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorUserID: pgUUID(actor),
		Action:      action,
		SubjectType: "oauth_app",
		SubjectID:   appID.String(),
		Data:        b,
	}); err != nil {
		return fmt.Errorf("writing audit log %s: %w", action, err)
	}
	return nil
}
