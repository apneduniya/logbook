package oauth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// scopeDescriptions are shown on the consent page.
var scopeDescriptions = map[string]string{
	ScopeProfile: "See your basic profile: your user id, username, email address and linked Hackatime account",
	ScopeRead:    "See your coding activity: projects, stats and heartbeats",
}

// AuthorizeParams is the query of GET /oauth/authorize.
type AuthorizeParams struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// Errors on the web UI's error page, for a bad client or redirect URI: the
// browser is never sent to a redirect URI we couldn't verify.
const (
	webErrInvalidClient      = "invalid_client"
	webErrInvalidRedirectURI = "invalid_redirect_uri"
)

// Authorize handles GET /oauth/authorize and returns where to send the
// browser. user is uuid.Nil when nobody is signed in.
//
//   - An unknown client or unregistered redirect URI goes to the web UI's
//     error page, never to the redirect URI.
//   - Other errors go to the redirect URI with error and state (RFC 6749
//     §4.1.2.1).
//   - A signed-in user who already granted these scopes to the app gets a
//     code straight away, as Doorkeeper does.
//   - Otherwise a pending request is stored and the browser goes to the
//     web UI's consent page.
func (s *Service) Authorize(ctx context.Context, p AuthorizeParams, user uuid.UUID) (string, error) {
	appID, err := uuid.Parse(p.ClientID)
	if err != nil {
		return s.webError(webErrInvalidClient), nil
	}
	app, err := s.st.OAuthAppByID(ctx, appID)
	if store.IsNotFound(err) {
		return s.webError(webErrInvalidClient), nil
	}
	if err != nil {
		return "", fmt.Errorf("reading app: %w", err)
	}
	if p.RedirectURI == "" || !slices.Contains(app.RedirectUris, p.RedirectURI) {
		return s.webError(webErrInvalidRedirectURI), nil
	}

	// From here on the redirect URI is verified, so errors go back to it.
	fail := func(code, desc string) (string, error) {
		v := url.Values{"error": {code}, "error_description": {desc}}
		if p.State != "" && len(p.State) <= maxStateLen {
			v.Set("state", p.State)
		}
		return withQuery(p.RedirectURI, v), nil
	}
	if p.ResponseType != "code" {
		return fail("unsupported_response_type", "response_type must be code")
	}
	if len(p.State) > maxStateLen {
		return fail("invalid_request", "state is too long")
	}
	scopes, ok := parseScopes(p.Scope, userScopes, ScopeProfile)
	if !ok {
		return fail("invalid_scope", "scope must be profile, read or both")
	}
	if p.CodeChallengeMethod != "" && p.CodeChallengeMethod != challengeMethodS256 {
		return fail("invalid_request", "code_challenge_method must be S256")
	}
	switch {
	case p.CodeChallenge != "" && p.CodeChallengeMethod == "":
		return fail("invalid_request", "code_challenge_method must be S256")
	case p.CodeChallenge == "" && p.CodeChallengeMethod != "":
		return fail("invalid_request", "code_challenge is missing")
	case p.CodeChallenge != "" && !validChallenge(p.CodeChallenge):
		return fail("invalid_request", "code_challenge is malformed")
	case p.CodeChallenge == "" && app.SecretHash == nil:
		return fail("invalid_request", "code_challenge is required for public clients (PKCE)")
	}

	pending := db.OAuthRequestInsertParams{
		AppID: app.ID, UserID: pgUUID(user), RedirectUri: p.RedirectURI, Scopes: scopes, State: p.State,
		CodeChallenge: p.CodeChallenge, CodeChallengeMethod: p.CodeChallengeMethod,
	}
	if user != uuid.Nil {
		redirect, granted, err := s.authorizeGranted(ctx, user, pending)
		if err != nil || granted {
			return redirect, err
		}
	}
	id := newSecret("")
	now := s.now()
	pending.IDHash = hash(id)
	pending.CreatedAt = now
	pending.ExpiresAt = now.Add(RequestTTL)
	if err := s.st.OAuthRequestInsert(ctx, pending); err != nil {
		return "", fmt.Errorf("storing consent request: %w", err)
	}
	return s.webURL + "/oauth/authorize?" + url.Values{"request": {id}}.Encode(), nil
}

// authorizeGranted issues a code without asking when user already granted
// at least these scopes to the app. granted is false when consent is needed.
func (s *Service) authorizeGranted(ctx context.Context, user uuid.UUID, p db.OAuthRequestInsertParams) (redirect string, granted bool, err error) {
	g, err := s.st.OAuthGrantActive(ctx, db.OAuthGrantActiveParams{UserID: user, AppID: p.AppID})
	if store.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading grant: %w", err)
	}
	if !covers(g.Scopes, p.Scopes) {
		return "", false, nil
	}
	var code string
	err = s.st.InTx(ctx, func(q *db.Queries) error {
		code, err = s.issueCode(ctx, q, user, p)
		return err
	})
	if err != nil {
		return "", false, err
	}
	return codeRedirect(p.RedirectUri, code, p.State), true, nil
}

// issueCode stores a new authorization code for an approved request.
func (s *Service) issueCode(ctx context.Context, q *db.Queries, user uuid.UUID, p db.OAuthRequestInsertParams) (string, error) {
	code := newSecret("")
	now := s.now()
	if err := q.OAuthCodeInsert(ctx, db.OAuthCodeInsertParams{
		CodeHash: hash(code), AppID: p.AppID, UserID: user, RedirectUri: p.RedirectUri, Scopes: p.Scopes,
		CodeChallenge: p.CodeChallenge, CodeChallengeMethod: p.CodeChallengeMethod,
		FamilyID: uuid.New(), CreatedAt: now, ExpiresAt: now.Add(CodeTTL),
	}); err != nil {
		return "", fmt.Errorf("storing code: %w", err)
	}
	return code, nil
}

func codeRedirect(redirectURI, code, state string) string {
	v := url.Values{"code": {code}}
	if state != "" {
		v.Set("state", state)
	}
	return withQuery(redirectURI, v)
}

// withQuery appends v to uri, keeping any query the registered URI has.
func withQuery(uri string, v url.Values) string {
	switch {
	case !strings.Contains(uri, "?"):
		return uri + "?" + v.Encode()
	case strings.HasSuffix(uri, "?") || strings.HasSuffix(uri, "&"):
		return uri + v.Encode()
	default:
		return uri + "&" + v.Encode()
	}
}

func (s *Service) webError(code string) string {
	return s.webURL + "/oauth/authorize?" + url.Values{"error": {code}}.Encode()
}

// ConsentApp is the app as the consent page shows it.
type ConsentApp struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Slug          string    `json:"slug"`
	HomepageURL   string    `json:"homepage_url"`
	OwnerUsername string    `json:"owner_username"`
}

// ScopeInfo is a scope with a plain description.
type ScopeInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ConsentRequest is a pending request as the consent page shows it.
type ConsentRequest struct {
	App         ConsentApp  `json:"app"`
	Scopes      []ScopeInfo `json:"scopes"`
	RedirectURI string      `json:"redirect_uri"`
	// AlreadyGranted is true when the user has granted these scopes to the
	// app before (for example, they signed in after starting). The page
	// may then approve without asking.
	AlreadyGranted bool      `json:"already_granted"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// requestHash decodes a consent request id; ok is false if it can't be one.
func requestHash(id string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(raw) != randomBytes {
		return nil, false
	}
	return hash(id), true
}

// ConsentRequest returns a pending request for user, binding it to user if
// nobody has opened it yet. Unknown, expired and other users' requests are
// all ErrNotFound.
func (s *Service) ConsentRequest(ctx context.Context, user uuid.UUID, id string) (ConsentRequest, error) {
	h, ok := requestHash(id)
	if !ok {
		return ConsentRequest{}, ErrNotFound
	}
	r, err := s.st.OAuthRequestClaim(ctx, db.OAuthRequestClaimParams{UserID: pgUUID(user), IDHash: h, Now: s.now()})
	if store.IsNotFound(err) {
		return ConsentRequest{}, ErrNotFound
	}
	if err != nil {
		return ConsentRequest{}, fmt.Errorf("reading consent request: %w", err)
	}
	app, err := s.st.OAuthAppByID(ctx, r.AppID)
	if store.IsNotFound(err) {
		return ConsentRequest{}, ErrNotFound
	}
	if err != nil {
		return ConsentRequest{}, fmt.Errorf("reading app: %w", err)
	}
	out := ConsentRequest{
		App: ConsentApp{
			ID: app.ID, Name: app.Name, Slug: app.Slug, HomepageURL: app.HomepageUrl, OwnerUsername: app.OwnerUsername,
		},
		Scopes:      make([]ScopeInfo, len(r.Scopes)),
		RedirectURI: r.RedirectUri,
		ExpiresAt:   r.ExpiresAt,
	}
	for i, sc := range r.Scopes {
		out.Scopes[i] = ScopeInfo{Name: sc, Description: scopeDescriptions[sc]}
	}
	g, err := s.st.OAuthGrantActive(ctx, db.OAuthGrantActiveParams{UserID: user, AppID: r.AppID})
	switch {
	case err == nil:
		out.AlreadyGranted = covers(g.Scopes, r.Scopes)
	case !store.IsNotFound(err):
		return ConsentRequest{}, fmt.Errorf("reading grant: %w", err)
	}
	return out, nil
}

// Decide approves or denies a pending request for user and returns the
// redirect URI to send the browser to: with a code, or with
// error=access_denied. A request can be decided once. Approval records the
// grant (scopes add to any the user granted the app before) and is audited
// oauth.grant.
func (s *Service) Decide(ctx context.Context, user uuid.UUID, id string, approve bool) (string, error) {
	h, ok := requestHash(id)
	if !ok {
		return "", ErrNotFound
	}
	var redirect string
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		r, err := q.OAuthRequestConsume(ctx, db.OAuthRequestConsumeParams{IDHash: h, Now: s.now(), UserID: pgUUID(user)})
		if store.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("consuming consent request: %w", err)
		}
		if _, err := q.OAuthAppByID(ctx, r.AppID); err != nil {
			if store.IsNotFound(err) {
				return ErrNotFound
			}
			return fmt.Errorf("reading app: %w", err)
		}
		if !approve {
			v := url.Values{"error": {"access_denied"}, "error_description": {"the user denied access"}}
			if r.State != "" {
				v.Set("state", r.State)
			}
			redirect = withQuery(r.RedirectUri, v)
			return nil
		}
		scopes, err := s.recordGrant(ctx, q, user, r.AppID, r.Scopes)
		if err != nil {
			return err
		}
		code, err := s.issueCode(ctx, q, user, db.OAuthRequestInsertParams{
			AppID: r.AppID, RedirectUri: r.RedirectUri, Scopes: r.Scopes,
			CodeChallenge: r.CodeChallenge, CodeChallengeMethod: r.CodeChallengeMethod,
		})
		if err != nil {
			return err
		}
		redirect = codeRedirect(r.RedirectUri, code, r.State)
		return audit(ctx, q, user, "oauth.grant", r.AppID, map[string]any{
			"scopes": r.Scopes, "granted_scopes": scopes,
		})
	})
	if err != nil {
		return "", err
	}
	return redirect, nil
}

// recordGrant adds scopes to user's grant for the app (a revoked grant
// starts over) and returns the grant's scopes.
func (s *Service) recordGrant(ctx context.Context, q *db.Queries, user, appID uuid.UUID, scopes []string) ([]string, error) {
	merged := scopes
	g, err := q.OAuthGrantForUpdate(ctx, db.OAuthGrantForUpdateParams{UserID: user, AppID: appID})
	switch {
	case err == nil && g.RevokedAt == nil:
		merged = canonicalScopes(append(slices.Clone(g.Scopes), scopes...))
	case err != nil && !store.IsNotFound(err):
		return nil, fmt.Errorf("reading grant: %w", err)
	}
	if err := q.OAuthGrantUpsert(ctx, db.OAuthGrantUpsertParams{UserID: user, AppID: appID, Scopes: merged, Now: s.now()}); err != nil {
		return nil, fmt.Errorf("recording grant: %w", err)
	}
	return merged, nil
}
