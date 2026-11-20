// Package statstest seeds users and user_minutes rows for tests of the
// stats, projects, profiles and live packages, and provides a stub session
// middleware. It is only imported from _test.go files.
package statstest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store"
)

// UserHeader is the request header StubSession reads the caller's id from.
const UserHeader = "X-Test-User"

// User inserts a user and returns its id. Rows are inserted with raw SQL
// because creating users belongs to the auth module.
func User(t testing.TB, st *store.Store, username, timezone string, public bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := st.Pool.QueryRow(context.Background(), `
		INSERT INTO users (hca_id, username, display_name, email, timezone, profile_public)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		"hca-"+username, username, "Name "+username, username+"@example.com", timezone, public,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seeding user %s: %v", username, err)
	}
	return id
}

// Minute describes one user_minutes row. Empty strings get defaults.
type Minute struct {
	At       time.Time
	Project  string
	Language string
	Editor   string
	Category string
	Label    string
	Seconds  int
}

// Minutes inserts rows for userID.
func Minutes(t testing.TB, st *store.Store, userID uuid.UUID, ms ...Minute) {
	t.Helper()
	for _, m := range ms {
		m = withDefaults(m)
		_, err := st.Pool.Exec(context.Background(), `
			INSERT INTO user_minutes (user_id, minute, project, language, editor, category, label, seconds)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			userID, m.At.UTC().Truncate(time.Minute), m.Project, m.Language, m.Editor, m.Category, m.Label, m.Seconds)
		if err != nil {
			t.Fatalf("seeding minute %v: %v", m.At, err)
		}
	}
}

// Span inserts n consecutive full minutes starting at from.
func Span(t testing.TB, st *store.Store, userID uuid.UUID, from time.Time, n int, tmpl Minute) {
	t.Helper()
	ms := make([]Minute, n)
	for i := range ms {
		m := tmpl
		m.At = from.Add(time.Duration(i) * time.Minute)
		if m.Seconds == 0 {
			m.Seconds = 60
		}
		ms[i] = m
	}
	Minutes(t, st, userID, ms...)
}

func withDefaults(m Minute) Minute {
	if m.Project == "" {
		m.Project = "logbook"
	}
	if m.Language == "" {
		m.Language = "Go"
	}
	if m.Editor == "" {
		m.Editor = "vscode"
	}
	if m.Category == "" {
		m.Category = "coding"
	}
	if m.Label == "" {
		m.Label = "you"
	}
	return m
}

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

// Request builds a request authenticated as userID (uuid.Nil for none).
func Request(t testing.TB, method, target string, userID uuid.UUID, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	if userID != uuid.Nil {
		r.Header.Set(UserHeader, userID.String())
	}
	return r
}
