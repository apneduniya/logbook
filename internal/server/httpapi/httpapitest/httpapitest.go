// Package httpapitest provides a stub session middleware and a request
// builder for HTTP handler tests, standing in for package auth (which
// feature packages never import). It is only imported from _test.go files.
package httpapitest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// UserHeader is the request header StubSession reads the caller's id from.
const UserHeader = "X-Test-User"

// StubSession authenticates requests by the UserHeader value, standing in
// for auth.Service.RequireSession. Requests without it get 401.
func StubSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.Header.Get(UserHeader))
		if err != nil {
			httpapi.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		ctx := httpapi.WithUser(r.Context(), httpapi.CurrentUser{ID: id, Role: "user", Via: "session"})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// NoLimit is a pass-through middleware for routes that take a rate limiter.
func NoLimit(next http.Handler) http.Handler { return next }

// Request builds a request authenticated as userID (uuid.Nil for none).
func Request(t testing.TB, method, target string, userID uuid.UUID, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	if userID != uuid.Nil {
		r.Header.Set(UserHeader, userID.String())
	}
	return r
}
