package localdb

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Default bounds. Options override them (tests use small values).
const (
	DefaultQueueCap     = 200_000
	DefaultEventCap     = 3_000_000 // ~90 days of 5 s activity samples plus headroom
	DefaultRetention    = 90 * 24 * time.Hour
	DefaultLogRetention = 48 * time.Hour
)

// ErrNewerSchema means the database was written by a newer agent.
var ErrNewerSchema = errors.New("local database was created by a newer logbook; update the agent")

// DB is the agent's local SQLite database. It is safe for concurrent use.
type DB struct {
	sql          *sql.DB
	log          *slog.Logger
	now          func() time.Time
	queueCap     int64
	eventCap     int64
	retention    time.Duration
	logRetention time.Duration
}

// Option configures a DB.
type Option func(*DB)

// WithLogger sets the logger used for dropped-row warnings.
func WithLogger(l *slog.Logger) Option { return func(d *DB) { d.log = l } }

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(d *DB) { d.now = now } }

// WithQueueCap sets the maximum rows kept in each upload queue.
func WithQueueCap(n int64) Option { return func(d *DB) { d.queueCap = n } }

// WithEventCap sets the maximum rows kept in each raw event table.
func WithEventCap(n int64) Option { return func(d *DB) { d.eventCap = n } }

// WithRetention sets how long raw events are kept.
func WithRetention(r time.Duration) Option { return func(d *DB) { d.retention = r } }

// Open opens (creating if needed) the database at path and applies pending
// migrations. The parent directory is created with mode 0700 and the file is
// restricted to 0600.
func Open(ctx context.Context, path string, opts ...Option) (*DB, error) {
	d := &DB{
		log: slog.New(slog.DiscardHandler), now: time.Now,
		queueCap: DefaultQueueCap, eventCap: DefaultEventCap,
		retention: DefaultRetention, logRetention: DefaultLogRetention,
	}
	for _, o := range opts {
		o(d)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating database dir: %w", err)
	}
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("database path %q must not contain '?'", path)
	}
	// _txlock=immediate takes the write lock at BEGIN, so concurrent writers
	// wait on busy_timeout instead of failing to upgrade a read lock.
	dsn := path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate" +
		"&_pragma=cache_size(-1024)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// A few connections are plenty for one desktop agent and keep memory low.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	d.sql = db

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("restricting database permissions: %w", err)
	}
	if err := d.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error {
	if err := d.sql.Close(); err != nil {
		return fmt.Errorf("closing database: %w", err)
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q has no version prefix", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		b, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// migrate applies every embedded migration newer than the database's
// version, each in its own transaction. Migrations are forward-only.
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, qCreateMigrations); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	var current int
	if err := d.sql.QueryRowContext(ctx, qCurrentVersion).Scan(&current); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(ms) > 0 && current > ms[len(ms)-1].version {
		return fmt.Errorf("%w (schema %d, agent knows %d)", ErrNewerSchema, current, ms[len(ms)-1].version)
	}
	for _, m := range ms {
		if m.version <= current {
			continue
		}
		err := d.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("applying %s: %w", m.name, err)
			}
			if _, err := tx.ExecContext(ctx, qInsertMigration, m.version, d.now().UnixMilli()); err != nil {
				return fmt.Errorf("recording %s: %w", m.name, err)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("migrating local database: %w", err)
		}
	}
	return nil
}

// inTx runs fn in a transaction, committing if it returns nil.
func (d *DB) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback() // the fn error is the one worth reporting
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

// queryer runs queries: *sql.DB or *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryRows runs query on q and calls scan for every row.
func queryRows(ctx context.Context, q queryer, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
