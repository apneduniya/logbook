package httpapi

import (
	"context"
	"net/http"
	"slices"

	"github.com/google/uuid"
)

// OAuthToken describes the OAuth access token that authenticated a request
// (ADR 0021). The oauth package's bearer middleware puts it on the context;
// handlers read it with OAuthTokenFrom.
type OAuthToken struct {
	// ID is the token's row id, stable for the token's life (rate-limit key).
	ID uuid.UUID
	// AppID is the OAuth app (program) the token was issued to.
	AppID uuid.UUID
	// UserID is the user who granted the token, or uuid.Nil for a
	// client-credentials token, which acts as the app itself.
	UserID uuid.UUID
	// Scopes the token carries, in canonical order.
	Scopes []string
}

// HasScope reports whether the token carries scope.
func (t OAuthToken) HasScope(scope string) bool { return slices.Contains(t.Scopes, scope) }

// IsClient reports whether the token is a client-credentials token, issued
// to the app itself rather than on behalf of a user.
func (t OAuthToken) IsClient() bool { return t.UserID == uuid.Nil }

type oauthTokenKey struct{}

// WithOAuthToken stores the request's OAuth token on the context. Only the
// oauth bearer middleware calls it.
func WithOAuthToken(ctx context.Context, t OAuthToken) context.Context {
	return context.WithValue(ctx, oauthTokenKey{}, t)
}

// OAuthTokenFrom returns the OAuth token that authenticated the request, if any.
func OAuthTokenFrom(ctx context.Context) (OAuthToken, bool) {
	t, ok := ctx.Value(oauthTokenKey{}).(OAuthToken)
	return t, ok
}

// OAuthTokenKey is a key function for RateLimiter.Middleware that limits
// per OAuth access token. Put it after the bearer middleware; a request
// without a token is not limited here.
func OAuthTokenKey(r *http.Request) string {
	if t, ok := OAuthTokenFrom(r.Context()); ok {
		return "token:" + t.ID.String()
	}
	return ""
}
