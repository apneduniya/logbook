// Package migrations embeds the goose SQL migrations so the server binary can
// bring its database up to date on start.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var files embed.FS

// Up applies all pending migrations.
func Up(ctx context.Context, db *sql.DB) error {
	p, err := goose.NewProvider(goose.DialectPostgres, db, files)
	if err != nil {
		return fmt.Errorf("building migration provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}
