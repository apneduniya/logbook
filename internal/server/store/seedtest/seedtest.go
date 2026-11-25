// Package seedtest inserts users, user_minutes rows, devices, credited
// minutes, heartbeats, Hackatime links and project settings, and reads the
// audit log, for tests of packages that read those tables (stats, projects,
// profiles, live, programapi). It uses the SeedTest* queries in
// store/queries/seedtest.sql and is only imported from _test.go files.
// Creating users in production belongs to auth, writing minutes and
// heartbeats to ingest, devices and credited minutes to devices and
// credited, links to hackatime, and settings to projects.
package seedtest

import (
	"context"
	"crypto/rand"
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

// DeviceEpoch is when Device registers devices: early enough that tests
// with fixed clocks can upload minutes and evidence for any recent day.
var DeviceEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// Device inserts an active device for userID, registered at DeviceEpoch,
// with a fresh random public key and returns its id.
func Device(t testing.TB, st *store.Store, userID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	return DeviceAt(t, st, userID, name, DeviceEpoch)
}

// DeviceAt is Device with an explicit registration time.
func DeviceAt(t testing.TB, st *store.Store, userID uuid.UUID, name string, registered time.Time) uuid.UUID {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key) // crypto/rand.Read never returns an error
	id, err := st.SeedTestDevice(context.Background(), db.SeedTestDeviceParams{
		UserID: userID, Name: name, PublicKey: key, CreatedAt: registered,
	})
	if err != nil {
		t.Fatalf("seeding device %s: %v", name, err)
	}
	return id
}

// Credited describes credited_minutes rows. Empty fields get defaults:
// project "logbook", label "you"; Seconds is stored as given.
type Credited struct {
	Project string
	Label   string
	Seconds int16
	Flags   []string
}

// CreditedSpan inserts n consecutive credited minutes for one device,
// starting at from, each with tmpl's fields.
func CreditedSpan(t testing.TB, st *store.Store, userID, deviceID uuid.UUID, from time.Time, n int, tmpl Credited) {
	t.Helper()
	if tmpl.Project == "" {
		tmpl.Project = "logbook"
	}
	if tmpl.Label == "" {
		tmpl.Label = "you"
	}
	if tmpl.Flags == nil {
		tmpl.Flags = []string{}
	}
	for i := range n {
		at := from.UTC().Truncate(time.Minute).Add(time.Duration(i) * time.Minute)
		err := st.SeedTestCreditedMinute(context.Background(), db.SeedTestCreditedMinuteParams{
			DeviceID: deviceID, UserID: userID, Minute: at, Project: tmpl.Project,
			Label: tmpl.Label, Credited: tmpl.Seconds, Flags: tmpl.Flags,
		})
		if err != nil {
			t.Fatalf("seeding credited minute %v: %v", at, err)
		}
	}
}

// Heartbeat describes one heartbeats row. Empty fields get defaults:
// project "logbook", language "Go", editor "vscode", category "coding",
// entity "main.go". The source is always "editor".
type Heartbeat struct {
	At       time.Time
	Project  string
	Language string
	Editor   string
	Category string
	Entity   string
}

// Heartbeats inserts heartbeats for userID with random fields hashes.
func Heartbeats(t testing.TB, st *store.Store, userID uuid.UUID, hs ...Heartbeat) {
	t.Helper()
	for _, h := range hs {
		m := withDefaults(Minute{Project: h.Project, Language: h.Language, Editor: h.Editor, Category: h.Category})
		if h.Entity == "" {
			h.Entity = "main.go"
		}
		hash := make([]byte, 32)
		_, _ = rand.Read(hash) // crypto/rand.Read never returns an error
		err := st.SeedTestHeartbeat(context.Background(), db.SeedTestHeartbeatParams{
			UserID: userID, Time: h.At.UTC(), FieldsHash: hash, Entity: h.Entity,
			Category: m.Category, Project: m.Project, Language: m.Language, Editor: m.Editor,
		})
		if err != nil {
			t.Fatalf("seeding heartbeat %v: %v", h.At, err)
		}
	}
}

// HackatimeLink links userID to Hackatime account hackatimeID with the given
// trust level and GitHub username. The stored token is a placeholder.
func HackatimeLink(t testing.TB, st *store.Store, userID uuid.UUID, hackatimeID int64, github, trust string) {
	t.Helper()
	err := st.SeedTestHackatimeLink(context.Background(), db.SeedTestHackatimeLinkParams{
		UserID: userID, HackatimeUserID: hackatimeID, HackatimeUsername: github, TrustLevel: trust,
	})
	if err != nil {
		t.Fatalf("seeding hackatime link: %v", err)
	}
}

// ProjectSettings stores settings for one of userID's projects.
func ProjectSettings(t testing.TB, st *store.Store, userID uuid.UUID, name, repoURL string, hidden, archived bool) {
	t.Helper()
	err := st.SeedTestProjectSettings(context.Background(), db.SeedTestProjectSettingsParams{
		UserID: userID, Name: name, RepoUrl: repoURL, Hidden: hidden, Archived: archived,
	})
	if err != nil {
		t.Fatalf("seeding project settings: %v", err)
	}
}

// PublicID returns userID's public id.
func PublicID(t testing.TB, st *store.Store, userID uuid.UUID) int64 {
	t.Helper()
	id, err := st.SeedTestPublicID(context.Background(), userID)
	if err != nil {
		t.Fatalf("reading public id: %v", err)
	}
	return id
}
