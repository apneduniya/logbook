// Package storetest gives each test its own freshly migrated Postgres
// database, cloned from a template, so test packages can run in parallel
// without sharing state. It needs DATABASE_URL pointing at a dev server
// (make db-up); tests are skipped when it is unset.
package storetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

	"github.com/apneduniya/logbook/internal/server/jobs"
	"github.com/apneduniya/logbook/internal/server/store"
)

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
	templateDB := templateName(t)
	ensureTemplate(ctx, t, base, templateDB)
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

	return open(ctx, t, base, name)
}

// open opens the test database name and drops it when the test ends.
func open(ctx context.Context, t testing.TB, base, name string) *store.Store {
	t.Helper()
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

// Partial is a new test database migrated only up to some version, for
// testing a migration against rows that exist before it runs. It is
// dropped when the test ends. River's tables are not added.
type Partial struct {
	Store *store.Store
	url   string
}

// NewPartial returns a database migrated up to and including version.
func NewPartial(t testing.TB, version int64) *Partial {
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
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "logbook_test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	p := &Partial{Store: open(ctx, t, base, name), url: withDB(t, base, name)}
	p.UpTo(t, version)
	return p
}

// UpTo applies the migrations up to and including version.
func (p *Partial) UpTo(t testing.TB, version int64) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", p.url)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpToContext(context.Background(), sqlDB, migrationsDir(t), version); err != nil {
		t.Fatalf("migrating to %d: %v", version, err)
	}
}

// ensureTemplate creates the template database if needed and migrates it to
// the latest version. The caller must hold the template advisory lock.
func ensureTemplate(ctx context.Context, t testing.TB, base, templateDB string) {
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
	migrateRiver(ctx, t, withDB(t, base, templateDB))
}

// migrateRiver adds River's job tables, which live outside goose (ADR 0002).
func migrateRiver(ctx context.Context, t testing.TB, url string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("opening template pool: %v", err)
	}
	defer pool.Close() // must close before the template is copied
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrating river in template: %v", err)
	}
}

// templateName is unique per set of migration files and River schema
// version, so worktrees or branches with different migrations never share
// (and corrupt) a template.
func templateName(t testing.TB) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(migrationsDir(t), "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("listing migrations: %v", err)
	}
	riverVersion, err := jobs.MigrationVersion()
	if err != nil {
		t.Fatalf("river migration version: %v", err)
	}
	h := sha256.New()
	h.Write([]byte("river:" + strconv.Itoa(riverVersion) + "\n"))
	for _, f := range files { // Glob returns sorted names
		b, err := os.ReadFile(f) //nolint:gosec // test helper reading repo files
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		h.Write([]byte(filepath.Base(f)))
		h.Write(b)
	}
	return "logbook_tpl_" + hex.EncodeToString(h.Sum(nil))[:16]
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
