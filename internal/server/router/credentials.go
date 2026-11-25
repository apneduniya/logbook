package router

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/auth"
	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/programapi"
)

// BearerChecker resolves OAuth access tokens; *oauth.Service implements it.
type BearerChecker interface {
	AuthenticateBearer(ctx context.Context, raw string) (oauth.Authenticated, error)
}

// KeyChecker resolves user API keys; *auth.Service implements it.
type KeyChecker interface {
	APIKeyOwner(ctx context.Context, key string) (uuid.UUID, error)
}

// ProgramCredentials adapts the OAuth provider and API keys to the program
// API's Authenticator.
type ProgramCredentials struct {
	OAuth BearerChecker
	Keys  KeyChecker
}

var _ programapi.Authenticator = ProgramCredentials{}

// AccessToken resolves an OAuth access token a user granted. A
// client-credentials token acts as the app, not as a user, so it reads no
// user data: it is refused like an invalid token.
func (c ProgramCredentials) AccessToken(ctx context.Context, token string) (programapi.Grant, error) {
	a, err := c.OAuth.AuthenticateBearer(ctx, token)
	if errors.Is(err, oauth.ErrInvalidToken) {
		return programapi.Grant{}, programapi.ErrInvalidCredentials
	}
	if err != nil {
		return programapi.Grant{}, fmt.Errorf("checking access token: %w", err)
	}
	if a.User == nil || a.Token.IsClient() {
		return programapi.Grant{}, programapi.ErrInvalidCredentials
	}
	return programapi.Grant{UserID: a.User.ID, AppID: a.Token.AppID, Scopes: a.Token.Scopes}, nil
}

// APIKey resolves a user API key to its owner.
func (c ProgramCredentials) APIKey(ctx context.Context, key string) (uuid.UUID, error) {
	id, err := c.Keys.APIKeyOwner(ctx, key)
	if errors.Is(err, auth.ErrInvalidAPIKey) {
		return uuid.Nil, programapi.ErrInvalidCredentials
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("checking API key: %w", err)
	}
	return id, nil
}
