package programapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// OAuth scopes (ADR 0021).
const (
	ScopeProfile = "profile"
	ScopeRead    = "read"
)

// ErrInvalidCredentials is what an Authenticator returns for a token or key
// that is unknown, malformed, expired or revoked.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Grant is a valid OAuth access token: whose data it reads, for which app,
// with which scopes.
type Grant struct {
	UserID uuid.UUID
	AppID  uuid.UUID
	Scopes []string
}

// Has reports whether the grant includes scope.
func (g Grant) Has(scope string) bool { return slices.Contains(g.Scopes, scope) }

// Authenticator resolves the credentials programs send. The server wires it
// to package oauth (access tokens) and package auth (user API keys); tests
// use a fake. Both methods return ErrInvalidCredentials for anything that
// doesn't verify, and another error only when the lookup itself failed.
type Authenticator interface {
	// AccessToken resolves an OAuth access token.
	AccessToken(ctx context.Context, token string) (Grant, error)
	// APIKey resolves a user API key (the key editor plugins use) to its
	// owner. Only the calls where Hackatime accepts its API key use it:
	// the users/{id}/* stats-controller calls and my/heartbeats.
	APIKey(ctx context.Context, key string) (uuid.UUID, error)
}

// authorization splits the Authorization header as Hackatime does
// (split(/\s+/, 2)): the scheme and the rest.
func authorization(r *http.Request) (scheme, cred string) {
	h := strings.TrimLeft(r.Header.Get("Authorization"), " \t")
	scheme, cred, _ = strings.Cut(h, " ")
	return scheme, strings.TrimLeft(cred, " \t")
}

// bearerToken returns the Bearer credential, if any.
func bearerToken(r *http.Request) (string, bool) {
	scheme, cred := authorization(r)
	if !strings.EqualFold(scheme, "Bearer") || cred == "" {
		return "", false
	}
	return cred, true
}

// basicKey returns the strictly base64-decoded Basic credential, used whole
// as an API key (Hackatime's api_key_token_from with sources [:basic]).
func basicKey(r *http.Request) (string, bool) {
	scheme, cred := authorization(r)
	if !strings.EqualFold(scheme, "Basic") || cred == "" {
		return "", false
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(cred)
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

// credential is who a request authenticated as.
type credential struct {
	userID uuid.UUID // uuid.Nil when anonymous
	grant  *Grant    // set for OAuth tokens
	secret string    // the token or key, for the rate-limit key only
}

func (c credential) anonymous() bool { return c.userID == uuid.Nil }

// userCredential resolves the credentials Hackatime's
// api_user_from_credentials(oauth_scopes: ["read"]) accepts: a user API key
// (Bearer, plus Basic when allowBasic), else an OAuth token that has the
// read scope. Anything that doesn't verify is anonymous, never an error.
func userCredential(ctx context.Context, a Authenticator, r *http.Request, allowBasic bool) (credential, error) {
	key, isBearer := bearerToken(r)
	if !isBearer && allowBasic {
		key, _ = basicKey(r)
	}
	if key == "" {
		return credential{}, nil
	}
	id, err := a.APIKey(ctx, key)
	switch {
	case err == nil:
		return credential{userID: id, secret: key}, nil
	case !errors.Is(err, ErrInvalidCredentials):
		return credential{}, err
	}
	if !isBearer {
		return credential{}, nil
	}
	g, err := a.AccessToken(ctx, key)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		return credential{}, nil
	case err != nil:
		return credential{}, err
	case !g.Has(ScopeRead):
		return credential{}, nil
	}
	return credential{userID: g.UserID, grant: &g, secret: key}, nil
}
