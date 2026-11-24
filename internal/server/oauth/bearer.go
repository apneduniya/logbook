package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// ErrInvalidToken means a bearer token is missing, malformed, unknown,
// expired, revoked, or belongs to a deleted app.
var ErrInvalidToken = errors.New("invalid access token")

// Authenticated is what an access token resolves to.
type Authenticated struct {
	Token httpapi.OAuthToken
	// User is the user who granted the token; nil for a client-credentials
	// token, which acts as the app itself.
	User *httpapi.CurrentUser
}

// AuthenticateBearer resolves a raw access token. Only OAuth access tokens
// are accepted: user API keys, refresh tokens and client secrets are all
// ErrInvalidToken.
func (s *Service) AuthenticateBearer(ctx context.Context, raw string) (Authenticated, error) {
	if !wellFormed(raw, accessPrefix) {
		return Authenticated{}, ErrInvalidToken
	}
	row, err := s.st.OAuthTokenByAccess(ctx, db.OAuthTokenByAccessParams{AccessHash: hash(raw), Now: s.now()})
	if store.IsNotFound(err) {
		return Authenticated{}, ErrInvalidToken
	}
	if err != nil {
		return Authenticated{}, fmt.Errorf("looking up access token: %w", err)
	}
	out := Authenticated{Token: httpapi.OAuthToken{
		ID: row.ID, AppID: row.AppID, UserID: fromPG(row.UserID), Scopes: row.Scopes,
	}}
	if row.UserID.Valid {
		out.User = &httpapi.CurrentUser{
			ID: row.UserID.Bytes, Username: row.Username.String, Role: row.Role.String, Via: "oauth",
		}
	}
	return out, nil
}

// bearerFromRequest returns the token in "Authorization: Bearer <token>",
// the only form Doorkeeper accepts here (access_token_methods
// :from_bearer_authorization). ok is false without one.
func bearerFromRequest(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// withAuthenticated puts the token, and its user if it has one, on ctx.
func withAuthenticated(ctx context.Context, a Authenticated) context.Context {
	ctx = httpapi.WithOAuthToken(ctx, a.Token)
	if a.User != nil {
		ctx = httpapi.WithUser(ctx, *a.User)
	}
	return ctx
}

// RequireScope returns middleware that lets a request through only with a
// valid OAuth access token carrying scope, sent as "Authorization: Bearer".
// It puts the token on the context (httpapi.OAuthTokenFrom) and, for a
// token a user granted, the user (httpapi.UserFrom, Via "oauth"). A
// client-credentials token has no user; only the submissions scope can
// pass with one.
//
// Sessions and user API keys never satisfy it. A missing or bad token is
// 401 and a token without the scope 403, each with a WWW-Authenticate
// header (RFC 6750 §3) and a JSON error body.
func (s *Service) RequireScope(scope string) httpapi.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerFromRequest(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="logbook"`)
				httpapi.WriteError(w, r, httpapi.Errorf(http.StatusUnauthorized, nil, "missing access token"))
				return
			}
			a, err := s.AuthenticateBearer(r.Context(), raw)
			if errors.Is(err, ErrInvalidToken) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="logbook", error="invalid_token"`)
				httpapi.WriteError(w, r, httpapi.Errorf(http.StatusUnauthorized, err, "invalid access token"))
				return
			}
			if err != nil {
				httpapi.WriteError(w, r, err)
				return
			}
			if !a.Token.HasScope(scope) {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="logbook", error="insufficient_scope", scope=%q`, scope))
				httpapi.WriteError(w, r, httpapi.Errorf(http.StatusForbidden, nil, "access token lacks the %s scope", scope))
				return
			}
			next.ServeHTTP(w, r.WithContext(withAuthenticated(r.Context(), a)))
		})
	}
}

// OptionalScope returns middleware for endpoints that are public but read
// more with a token, as Hackatime's stats endpoints are: a valid bearer
// token carrying scope is put on the context as RequireScope does; a
// missing, invalid or under-scoped token leaves the request anonymous
// (Hackatime's api_user_from_credentials ignores such tokens). It only
// fails on a database error.
func (s *Service) OptionalScope(scope string) httpapi.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerFromRequest(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			a, err := s.AuthenticateBearer(r.Context(), raw)
			switch {
			case errors.Is(err, ErrInvalidToken):
				next.ServeHTTP(w, r)
			case err != nil:
				httpapi.WriteError(w, r, err)
			case !a.Token.HasScope(scope):
				next.ServeHTTP(w, r)
			default:
				next.ServeHTTP(w, r.WithContext(withAuthenticated(r.Context(), a)))
			}
		})
	}
}
