package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type requestIDKey struct{}

// RequestID tags each request with a random id, exposed as X-Request-Id.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// RequestIDFrom returns the request id set by RequestID.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush on the real writer (needed for SSE).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Logger logs one line per request. It never logs query strings, bodies or
// headers, which may carry API keys, nor a path whose route pattern has a
// {token} segment (a review link's token is its credential): such a path is
// logged as its pattern.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.InfoContext(r.Context(), "http",
			"method", r.Method, "path", loggedPath(r), "status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(), "req_id", RequestIDFrom(r.Context()))
	})
}

// loggedPath is the request's path, or the path of its route pattern when
// the pattern names a {token}. ServeMux sets r.Pattern on the request it
// routes, which is the one Logger passed on.
func loggedPath(r *http.Request) string {
	if strings.Contains(r.Pattern, "{token}") {
		if _, path, ok := strings.Cut(r.Pattern, " "); ok {
			return path
		}
		return r.Pattern
	}
	return r.URL.Path
}

// Recover turns panics into 500 responses and logs the stack.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { //nolint:contextcheck // uses r.Context() via WriteError
			if p := recover(); p != nil {
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				slog.ErrorContext(r.Context(), "panic", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				WriteError(w, r, fmt.Errorf("panic: %v", p))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders sets conservative defaults on every response.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}
