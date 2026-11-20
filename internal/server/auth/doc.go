// Package auth owns who the caller is: sign-in with Hack Club Auth (OAuth 2
// with state and PKCE), server-side browser sessions with CSRF tokens, API
// keys for editor plugins and the agent, and a localhost-only dev sign-in.
//
// It exposes middleware (RequireSession, OptionalSession, RequireAPIKey) that
// puts an httpapi.CurrentUser on the request context. Feature packages take
// that middleware as a constructor argument and never import this package.
// It does not own user profiles beyond what sign-in needs, roles beyond
// reading them, or Hackatime account linking.
package auth
