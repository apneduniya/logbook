package router_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/auth"
	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/programapi"
	"github.com/apneduniya/logbook/internal/server/router"
)

type fakeBearer struct {
	a   oauth.Authenticated
	err error
}

func (f fakeBearer) AuthenticateBearer(context.Context, string) (oauth.Authenticated, error) {
	return f.a, f.err
}

type fakeKeys struct {
	id  uuid.UUID
	err error
}

func (f fakeKeys) APIKeyOwner(context.Context, string) (uuid.UUID, error) { return f.id, f.err }

func TestProgramCredentialsAccessToken(t *testing.T) {
	user, app := uuid.New(), uuid.New()
	userToken := oauth.Authenticated{
		Token: httpapi.OAuthToken{ID: uuid.New(), AppID: app, UserID: user, Scopes: []string{"profile", "read"}},
		User:  &httpapi.CurrentUser{ID: user, Via: "oauth"},
	}
	clientToken := oauth.Authenticated{Token: httpapi.OAuthToken{ID: uuid.New(), AppID: app, Scopes: []string{"submissions"}}}
	dbErr := errors.New("connection reset")

	tests := []struct {
		name    string
		bearer  fakeBearer
		want    programapi.Grant
		wantErr error
	}{
		{"user token", fakeBearer{a: userToken}, programapi.Grant{UserID: user, AppID: app, Scopes: []string{"profile", "read"}}, nil},
		{"client credentials", fakeBearer{a: clientToken}, programapi.Grant{}, programapi.ErrInvalidCredentials},
		{"invalid token", fakeBearer{err: oauth.ErrInvalidToken}, programapi.Grant{}, programapi.ErrInvalidCredentials},
		{"lookup failed", fakeBearer{err: dbErr}, programapi.Grant{}, dbErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := router.ProgramCredentials{OAuth: tc.bearer}.AccessToken(context.Background(), "lbo_x")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if errors.Is(err, dbErr) && errors.Is(err, programapi.ErrInvalidCredentials) {
				t.Fatal("a failed lookup must not read as an invalid token")
			}
			if got.UserID != tc.want.UserID || got.AppID != tc.want.AppID || len(got.Scopes) != len(tc.want.Scopes) {
				t.Fatalf("grant = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProgramCredentialsAPIKey(t *testing.T) {
	owner := uuid.New()
	dbErr := errors.New("connection reset")
	tests := []struct {
		name    string
		keys    fakeKeys
		want    uuid.UUID
		wantErr error
	}{
		{"valid key", fakeKeys{id: owner}, owner, nil},
		{"invalid key", fakeKeys{err: auth.ErrInvalidAPIKey}, uuid.Nil, programapi.ErrInvalidCredentials},
		{"lookup failed", fakeKeys{err: dbErr}, uuid.Nil, dbErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := router.ProgramCredentials{Keys: tc.keys}.APIKey(context.Background(), "k")
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Fatalf("= %v, %v; want %v, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
