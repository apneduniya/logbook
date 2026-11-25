package oauth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// TokenError is an OAuth error response (RFC 6749 §5.2), rendered as
// {"error": Code, "error_description": Description}.
type TokenError struct {
	Code        string
	Description string
}

func (e *TokenError) Error() string { return e.Code + ": " + e.Description }

// Status is the HTTP status for the error: 401 for invalid_client, 400
// otherwise.
func (e *TokenError) Status() int {
	if e.Code == "invalid_client" {
		return http.StatusUnauthorized
	}
	return http.StatusBadRequest
}

func tokenErr(code, desc string) *TokenError { return &TokenError{Code: code, Description: desc} }

var (
	errInvalidClient = tokenErr("invalid_client", "client authentication failed")
	errBadCode       = tokenErr("invalid_grant", "the authorization code is invalid, expired or already used")
	errBadRefresh    = tokenErr("invalid_grant", "the refresh token is invalid, expired or revoked")
)

// Client is how a request to /oauth/token or /oauth/revoke identified its
// client: HTTP Basic or client_id and client_secret in the body.
type Client struct {
	ID     string
	Secret string
}

// TokenRequest is the form of POST /oauth/token.
type TokenRequest struct {
	GrantType    string
	Code         string
	RedirectURI  string
	CodeVerifier string
	RefreshToken string
	Scope        string
}

// TokenResponse is Doorkeeper's token response. RefreshToken is empty for
// client credentials.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
	CreatedAt    int64  `json:"created_at"`
}

// authenticateClient checks c against a live app. A confidential app needs
// its secret; a public client must not send one. Every failure is the same
// invalid_client.
func (s *Service) authenticateClient(ctx context.Context, c Client) (db.OAuthAppByIDRow, error) {
	id, err := uuid.Parse(c.ID)
	if err != nil {
		return db.OAuthAppByIDRow{}, errInvalidClient
	}
	app, err := s.st.OAuthAppByID(ctx, id)
	if store.IsNotFound(err) {
		return db.OAuthAppByIDRow{}, errInvalidClient
	}
	if err != nil {
		return db.OAuthAppByIDRow{}, fmt.Errorf("reading client: %w", err)
	}
	if app.SecretHash == nil {
		if c.Secret != "" {
			return db.OAuthAppByIDRow{}, errInvalidClient
		}
		return app, nil
	}
	if c.Secret == "" || subtle.ConstantTimeCompare(hash(c.Secret), app.SecretHash) != 1 {
		return db.OAuthAppByIDRow{}, errInvalidClient
	}
	return app, nil
}

// Token handles POST /oauth/token. Failures are *TokenError; anything else
// is an internal error.
func (s *Service) Token(ctx context.Context, c Client, req TokenRequest) (TokenResponse, error) {
	app, err := s.authenticateClient(ctx, c)
	if err != nil {
		return TokenResponse{}, err
	}
	switch req.GrantType {
	case "authorization_code":
		return s.exchangeCode(ctx, app, req)
	case "refresh_token":
		return s.refresh(ctx, app, req)
	case "client_credentials":
		return s.clientCredentials(ctx, app, req)
	case "":
		return TokenResponse{}, tokenErr("invalid_request", "grant_type is required")
	}
	return TokenResponse{}, tokenErr("unsupported_grant_type", "grant_type must be authorization_code, refresh_token or client_credentials")
}

// wellFormed reports whether v is prefix plus 32 bytes of base64url.
func wellFormed(v, prefix string) bool {
	rest, ok := strings.CutPrefix(v, prefix)
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(raw) == randomBytes
}

// exchangeCode redeems an authorization code (RFC 6749 §4.1.3, RFC 7636
// §4.6). The code is used up by any attempt from its own client, right or
// wrong; a second attempt revokes every token issued from it.
func (s *Service) exchangeCode(ctx context.Context, app db.OAuthAppByIDRow, req TokenRequest) (TokenResponse, error) {
	if req.Code == "" || req.RedirectURI == "" {
		return TokenResponse{}, tokenErr("invalid_request", "code and redirect_uri are required")
	}
	if !wellFormed(req.Code, "") {
		return TokenResponse{}, errBadCode
	}
	var out TokenResponse
	var failure *TokenError
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		// The family is locked before the code row, the order cleanup takes
		// them in (family lock, then codes), so the two can't deadlock.
		family, err := q.OAuthCodeFamily(ctx, hash(req.Code))
		if store.IsNotFound(err) {
			failure = errBadCode
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading code: %w", err)
		}
		if err := q.OAuthFamilyLock(ctx, family); err != nil {
			return fmt.Errorf("locking token family: %w", err)
		}
		now := s.now()
		c, err := q.OAuthCodeForUpdate(ctx, hash(req.Code))
		if store.IsNotFound(err) || (err == nil && c.AppID != app.ID) {
			// Not found: deleted by cleanup since it was read.
			failure = errBadCode
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking code: %w", err)
		}
		if c.UsedAt != nil {
			failure = errBadCode
			return s.revokeFamily(ctx, q, c.FamilyID, c.UserID, app.ID, "code_reuse")
		}
		if err := q.OAuthCodeMarkUsed(ctx, db.OAuthCodeMarkUsedParams{UsedAt: now, CodeHash: c.CodeHash}); err != nil {
			return fmt.Errorf("using code: %w", err)
		}
		if failure = checkCode(c, app, req, now); failure != nil {
			return nil // commit: the code stays used
		}
		active, err := grantCovers(ctx, q, c.UserID, app.ID, c.Scopes)
		if err != nil {
			return err
		}
		if !active {
			failure = errBadCode
			return nil
		}
		out, err = s.issueTokens(ctx, q, app.ID, c.UserID, c.Scopes, c.FamilyID)
		return err
	})
	if err != nil {
		return TokenResponse{}, err
	}
	if failure != nil {
		return TokenResponse{}, failure
	}
	return out, nil
}

// checkCode applies the rules of a code exchange once the code is claimed.
func checkCode(c db.OauthCode, app db.OAuthAppByIDRow, req TokenRequest, now time.Time) *TokenError {
	switch {
	case !now.Before(c.ExpiresAt):
		return errBadCode
	case c.RedirectUri != req.RedirectURI:
		return tokenErr("invalid_grant", "redirect_uri does not match the authorization request")
	case !slices.Contains(app.RedirectUris, c.RedirectUri):
		return tokenErr("invalid_grant", "redirect_uri is no longer registered")
	case c.CodeChallenge != "" && req.CodeVerifier == "":
		return tokenErr("invalid_grant", "code_verifier is required")
	case c.CodeChallenge != "" && !verifyPKCE(c.CodeChallenge, req.CodeVerifier):
		return tokenErr("invalid_grant", "code_verifier does not match the code_challenge")
	case c.CodeChallenge == "" && req.CodeVerifier != "":
		return tokenErr("invalid_grant", "code_verifier sent but the authorization request had no code_challenge")
	}
	return nil
}

// grantCovers reports whether user still has an active grant to the app
// that includes scopes.
func grantCovers(ctx context.Context, q *db.Queries, user, appID uuid.UUID, scopes []string) (bool, error) {
	g, err := q.OAuthGrantActive(ctx, db.OAuthGrantActiveParams{UserID: user, AppID: appID})
	if store.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading grant: %w", err)
	}
	return covers(g.Scopes, scopes), nil
}

// refresh redeems a refresh token (RFC 6749 §6): the old access and refresh
// tokens are revoked and a new pair is issued with the same or fewer
// scopes. A refresh token presented after it was rotated revokes its family.
//
// The grant is locked (FOR SHARE) before the token, the same order as
// RevokeGrant and app deletion take them. A revocation in flight therefore
// either commits first, and the refresh sees the grant revoked, or waits for
// the refresh to commit, and its token UPDATE then sees the new pair.
//
// Between the two, the token's family is locked (OAuthFamilyLock), which
// every issuance into a family and every family revocation takes. Refreshes
// share the grant lock, so without it a replay of an old refresh token could
// revoke the family with a snapshot taken before a concurrent refresh added
// its new pair, and that pair would survive. The family lock is always taken
// before any token row, so it can't deadlock with the row locks.
func (s *Service) refresh(ctx context.Context, app db.OAuthAppByIDRow, req TokenRequest) (TokenResponse, error) {
	if req.RefreshToken == "" {
		return TokenResponse{}, tokenErr("invalid_request", "refresh_token is required")
	}
	if !wellFormed(req.RefreshToken, refreshPrefix) {
		return TokenResponse{}, errBadRefresh
	}
	var out TokenResponse
	var failure *TokenError
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		owner, err := q.OAuthTokenOwnerByRefresh(ctx, hash(req.RefreshToken))
		if store.IsNotFound(err) || (err == nil && owner.AppID != app.ID) {
			failure = errBadRefresh
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading refresh token: %w", err)
		}
		user := fromPG(owner.UserID)
		grant, active, err := lockGrant(ctx, q, user, app.ID)
		if err != nil {
			return err
		}
		if err := q.OAuthFamilyLock(ctx, owner.FamilyID); err != nil {
			return fmt.Errorf("locking token family: %w", err)
		}
		now := s.now()
		t, err := q.OAuthTokenByRefreshForUpdate(ctx, hash(req.RefreshToken))
		if store.IsNotFound(err) {
			// Deleted by cleanup since it was read: the token is dead.
			failure = errBadRefresh
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking refresh token: %w", err)
		}
		if t.RevokedAt != nil {
			failure = errBadRefresh
			return s.revokeFamily(ctx, q, t.FamilyID, user, app.ID, "refresh_reuse")
		}
		if t.RefreshExpiresAt == nil || !now.Before(*t.RefreshExpiresAt) {
			failure = errBadRefresh
			return nil
		}
		scopes := t.Scopes
		if req.Scope != "" {
			narrowed, ok := parseScopes(req.Scope, t.Scopes, "")
			if !ok {
				failure = tokenErr("invalid_scope", "scope must not exceed the scopes originally granted")
				return nil
			}
			scopes = narrowed
		}
		if err := q.OAuthTokenRevoke(ctx, db.OAuthTokenRevokeParams{RevokedAt: now, ID: t.ID}); err != nil {
			return fmt.Errorf("revoking old token: %w", err)
		}
		if !active || !covers(grant.Scopes, scopes) {
			failure = errBadRefresh
			return nil
		}
		out, err = s.issueTokens(ctx, q, app.ID, user, scopes, t.FamilyID)
		return err
	})
	if err != nil {
		return TokenResponse{}, err
	}
	if failure != nil {
		return TokenResponse{}, failure
	}
	return out, nil
}

// lockGrant reads user's grant to the app, locking it against revocation
// until the transaction ends. active is false when there is no grant or it
// was revoked.
func lockGrant(ctx context.Context, q *db.Queries, user, appID uuid.UUID) (db.OauthGrant, bool, error) {
	g, err := q.OAuthGrantForShare(ctx, db.OAuthGrantForShareParams{UserID: user, AppID: appID})
	if store.IsNotFound(err) {
		return db.OauthGrant{}, false, nil
	}
	if err != nil {
		return db.OauthGrant{}, false, fmt.Errorf("locking grant: %w", err)
	}
	return g, g.RevokedAt == nil, nil
}

// clientCredentials issues the app a token for its own API calls (RFC 6749
// §4.4). Only confidential apps, and only the submissions scope.
func (s *Service) clientCredentials(ctx context.Context, app db.OAuthAppByIDRow, req TokenRequest) (TokenResponse, error) {
	if app.SecretHash == nil {
		return TokenResponse{}, tokenErr("unauthorized_client", "public clients can't use client_credentials")
	}
	scopes, ok := parseScopes(req.Scope, []string{ScopeSubmissions}, ScopeSubmissions)
	if !ok {
		return TokenResponse{}, tokenErr("invalid_scope", "client_credentials only grants the submissions scope")
	}
	access := newSecret(accessPrefix)
	now := s.now()
	if _, err := s.st.OAuthTokenInsert(ctx, db.OAuthTokenInsertParams{
		AccessHash: hash(access), AppID: app.ID, Scopes: scopes, FamilyID: uuid.New(),
		CreatedAt: now, ExpiresAt: now.Add(ClientTokenTTL),
	}); err != nil {
		return TokenResponse{}, fmt.Errorf("storing token: %w", err)
	}
	return TokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(ClientTokenTTL / time.Second),
		Scope: scopeString(scopes), CreatedAt: now.Unix(),
	}, nil
}

// issueTokens stores a new access and refresh token for user.
func (s *Service) issueTokens(ctx context.Context, q *db.Queries, appID, user uuid.UUID, scopes []string, family uuid.UUID) (TokenResponse, error) {
	access, refresh := newSecret(accessPrefix), newSecret(refreshPrefix)
	now := s.now()
	refreshExpiry := now.Add(RefreshTokenTTL)
	if _, err := q.OAuthTokenInsert(ctx, db.OAuthTokenInsertParams{
		AccessHash: hash(access), RefreshHash: hash(refresh), AppID: appID, UserID: pgUUID(user),
		Scopes: scopes, FamilyID: family, CreatedAt: now, ExpiresAt: now.Add(AccessTokenTTL),
		RefreshExpiresAt: &refreshExpiry,
	}); err != nil {
		return TokenResponse{}, fmt.Errorf("storing tokens: %w", err)
	}
	return TokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(AccessTokenTTL / time.Second),
		RefreshToken: refresh, Scope: scopeString(scopes), CreatedAt: now.Unix(),
	}, nil
}

// revokeFamily revokes every token from one authorization code after a
// replay, and audits it as oauth.revoke when that revoked anything (a code
// burned by a revoked grant has nothing left to revoke). The caller holds
// the family lock (OAuthFamilyLock), so no token joins the family after
// this UPDATE's snapshot.
func (s *Service) revokeFamily(ctx context.Context, q *db.Queries, family, user, appID uuid.UUID, reason string) error {
	n, err := q.OAuthTokensRevokeFamily(ctx, db.OAuthTokensRevokeFamilyParams{RevokedAt: s.now(), FamilyID: family})
	if err != nil {
		return fmt.Errorf("revoking token family: %w", err)
	}
	if n == 0 {
		return nil
	}
	return audit(ctx, q, user, "oauth.revoke", appID, map[string]any{
		"reason": reason, "tokens_revoked": n,
	})
}

// Revoke handles POST /oauth/revoke (RFC 7009). The client must
// authenticate; then any token string succeeds, and only a token issued to
// that client is actually revoked. Revoking either half of a pair revokes
// both. Audited oauth.revoke when a token was revoked.
func (s *Service) Revoke(ctx context.Context, c Client, token string) error {
	app, err := s.authenticateClient(ctx, c)
	if err != nil {
		return err
	}
	if !wellFormed(token, accessPrefix) && !wellFormed(token, refreshPrefix) {
		return nil
	}
	return s.st.InTx(ctx, func(q *db.Queries) error {
		row, err := q.OAuthTokenRevokeByHash(ctx, db.OAuthTokenRevokeByHashParams{
			RevokedAt: s.now(), AppID: app.ID, Hash: hash(token),
		})
		if store.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("revoking token: %w", err)
		}
		return audit(ctx, q, fromPG(row.UserID), "oauth.revoke", app.ID, map[string]any{
			"reason": "client_revoke", "token_id": row.ID,
		})
	})
}
