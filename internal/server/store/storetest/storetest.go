// Package storetest gives each test its own freshly migrated Postgres
// database, cloned from a template, so test packages can run in parallel
// without sharing state. It needs DATABASE_URL pointing at a dev server
// (make db-up); tests are skipped when it is unset.
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

	"github.com/apneduniya/logbook/internal/server/store"
)

const templateDB = "logbook_test_template"

// New returns a Store backed by a new database that is dropped when the test ends.
func New(t testing.TB) *store.Store {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set; run `make db-up`")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	// Hold a lock across migrating and copying the template: Postgres refuses
	// to copy a template while another package is still connected to it.
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock(727001)"); err != nil {
		t.Fatalf("locking: %v", err)
	}
	ensureTemplate(ctx, t, base)
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "logbook_test_" + hex.EncodeToString(b[:])
	_, createErr := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB))
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_unlock(727001)"); err != nil {
		t.Fatalf("unlocking: %v", err)
	}
	if createErr != nil {
		t.Fatalf("creating test database: %v", createErr)
	}

	st, err := store.Open(ctx, withDB(t, base, name))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Logf("dropping %s: %v", name, err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name)); err != nil {
			t.Logf("dropping %s: %v", name, err)
		}
	})
	return st
}

// ensureTemplate creates the template database if needed and migrates it to
// the latest version. The caller must hold the template advisory lock.
func ensureTemplate(ctx context.Context, t testing.TB, base string) {
	t.Helper()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", templateDB).Scan(&exists); err != nil {
		t.Fatalf("checking template: %v", err)
	}
	if !exists {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDB); err != nil {
			t.Fatalf("creating template: %v", err)
		}
	}
	// Always run migrations: goose is a no-op when the template is current.
	sqlDB, err := sql.Open("pgx", withDB(t, base, templateDB))
	if err != nil {
		t.Fatalf("opening template: %v", err)
	}
	defer func() { _ = sqlDB.Close() }() // closed explicitly below; this covers early t.Fatalf
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpContext(ctx, sqlDB, migrationsDir(t)); err != nil {
		t.Fatalf("migrating template: %v", err)
	}
	// Close the template's connections so CREATE DATABASE ... TEMPLATE can copy it.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("closing template: %v", err)
	}
}

func withDB(t testing.TB, base, name string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parsing DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func migrationsDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating migrations")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "migrations")
}
