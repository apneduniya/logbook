// Package httpapi holds the HTTP plumbing shared by every feature: JSON
// helpers, middleware, and the request-scoped current user. It contains no
// feature logic. Feature packages register their own routes on the mux via a
// Register(mux *http.ServeMux) method and are wired together in cmd/.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// MaxJSONBody caps request bodies decoded by DecodeJSON.
const MaxJSONBody = 1 << 20 // 1 MiB

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first listed runs outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Error is an error with an HTTP status and a message safe to show clients.
type Error struct {
	Status  int
	Message string
	Err     error // internal cause, logged but never sent
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d %s: %v", e.Status, e.Message, e.Err)
	}
	return fmt.Sprintf("%d %s", e.Status, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an *Error with a client-safe message.
func Errorf(status int, cause error, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...), Err: cause}
}

// HandlerFunc is an http.HandlerFunc that returns an error. Errors are
// rendered as JSON by Handle; unknown errors become 500s without detail.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Handle adapts a HandlerFunc to http.Handler.
func Handle(f HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := f(w, r); err != nil {
			WriteError(w, r, err)
		}
	})
}

// WriteJSON writes v with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("writing json response", "err", err)
	}
}

// WriteError renders err as {"error": "..."} and logs server errors.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var he *Error
	if !errors.As(err, &he) {
		he = &Error{Status: http.StatusInternalServerError, Message: "internal error", Err: err}
	}
	if he.Status >= 500 {
		slog.ErrorContext(r.Context(), "request failed", "err", err, "path", r.URL.Path)
	}
	WriteJSON(w, he.Status, map[string]string{"error": he.Message})
}

// DecodeJSON decodes a size-limited JSON body into v, rejecting unknown
// trailing data. Unknown fields are allowed for client compatibility.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return Errorf(http.StatusRequestEntityTooLarge, err, "request body too large")
		}
		return Errorf(http.StatusBadRequest, err, "invalid JSON body")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Errorf(http.StatusBadRequest, err, "unexpected data after JSON body")
	}
	return nil
}

// CurrentUser identifies the authenticated caller of a request.
type CurrentUser struct {
	ID       uuid.UUID
	Username string
	Role     string
	// Via is "session", "api_key" or "oauth" (an OAuth access token; see
	// OAuthTokenFrom for its app and scopes).
	Via string
}

type userKey struct{}

// WithUser stores the authenticated user on the context. Only auth middleware calls it.
func WithUser(ctx context.Context, u CurrentUser) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFrom returns the authenticated user, if any.
func UserFrom(ctx context.Context) (CurrentUser, bool) {
	u, ok := ctx.Value(userKey{}).(CurrentUser)
	return u, ok
}
