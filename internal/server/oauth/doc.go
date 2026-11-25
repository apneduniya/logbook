// Package oauth is Logbook's OAuth 2 provider (ADR 0021): the apps
// programs register, their members, the authorization-code grant with
// PKCE, refresh tokens, client credentials, revocation, and the grants a
// user has given.
//
// It owns:
//   - /api/v1/me/apps* (session): apps, secret rotation and members. Only
//     the owner writes; reviewers read. At most MaxAppsPerUser live apps per owner.
//   - GET /api/v1/programs: public apps, for the submit picker.
//   - GET /oauth/authorize, GET and POST /api/v1/oauth/requests/{id}: the
//     consent flow. The consent page itself is on the web UI.
//   - POST /oauth/token and POST /oauth/revoke (RFC 6749, RFC 7009), with
//     Doorkeeper's response shapes.
//   - GET /api/v1/me/grants and DELETE /api/v1/me/grants/{app_id}.
//   - The bearer middleware (RequireScope, OptionalScope) that other
//     packages use to accept access tokens. It puts the user on the context
//     with httpapi.WithUser and the token with httpapi.WithOAuthToken.
//   - The audit entries app.create, app.update, app.secret_rotate,
//     app.delete, app.member_add, app.member_remove, oauth.grant and
//     oauth.revoke.
//
// It does not own sessions or API keys (package auth; its middleware is
// injected), the program API read endpoints, or submissions and decisions.
//
// # Secrets
//
// Client secrets, codes, consent request ids, access and refresh tokens are
// 256-bit random values. Only their SHA-256 is stored, and each is shown
// once. Lookups are by hash; client secrets are compared in constant time.
//
// # Token families
//
// Every token issued from one authorization code, and every refresh of it,
// shares a family id. Presenting a used code or a rotated refresh token
// again revokes the whole family (RFC 6749 §4.1.2 and §10.4): one of the two
// presenters is an attacker, and we can't tell which.
package oauth
