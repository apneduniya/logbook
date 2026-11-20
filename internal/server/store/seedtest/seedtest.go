// Package seedtest inserts users and user_minutes rows and reads the audit
// log for tests of packages that read those tables (stats, projects,
// profiles, live). It uses the SeedTest* queries in
// store/queries/seedtest.sql and is only imported from _test.go files.
// Creating users in production belongs to auth, and writing minutes to ingest.
package seedtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// User inserts a user with a derived HCA id, display name ("Name <username>")
// and email ("<username>@example.com"), and returns its id.
func User(t testing.TB, st *store.Store, username, timezone string, public bool) uuid.UUID {
	t.Helper()
	id, err := st.SeedTestUser(context.Background(), db.SeedTestUserParams{
		HcaID: "hca-" + username, Username: username, DisplayName: "Name " + username,
		Email: username + "@example.com", Timezone: timezone, ProfilePublic: public,
	})
	if err != nil {
		t.Fatalf("seeding user %s: %v", username, err)
	}
	return id
}

// Minute describes one user_minutes row. Empty fields get defaults:
// project "logbook", language "Go", editor "vscode", category "coding",
// label "you".
type Minute struct {
	At       time.Time
	Project  string
	Language string
	Editor   string
	Category string
	Label    string
	Seconds  int16
}

// Minutes inserts rows for userID.
func Minutes(t testing.TB, st *store.Store, userID uuid.UUID, ms ...Minute) {
	t.Helper()
	for _, m := range ms {
		m = withDefaults(m)
		err := st.SeedTestUserMinute(context.Background(), db.SeedTestUserMinuteParams{
			UserID: userID, Minute: m.At.UTC().Truncate(time.Minute),
			Project: m.Project, Language: m.Language, Editor: m.Editor,
			Category: m.Category, Label: m.Label, Seconds: m.Seconds,
		})
		if err != nil {
			t.Fatalf("seeding minute %v: %v", m.At, err)
		}
	}
}

// Span inserts n consecutive minutes starting at from, each with tmpl's
// fields; Seconds defaults to a full 60.
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

// AuditEntry is one audit_log row as tests compare it.
type AuditEntry = db.SeedTestAuditLogRow

// Audit returns the audit rows written by actor, oldest first.
func Audit(t testing.TB, st *store.Store, actor uuid.UUID) []AuditEntry {
	t.Helper()
	rows, err := st.SeedTestAuditLog(context.Background(), pgtype.UUID{Bytes: actor, Valid: true})
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	return rows
}
