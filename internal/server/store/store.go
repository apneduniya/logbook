// Package store owns the server's Postgres access: the connection pool,
// transactions and the sqlc-generated queries in store/db. Services depend on
// the db.Querier interface, never on pgx directly.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Store bundles the pool with the generated queries.
type Store struct {
	Pool *pgxpool.Pool
	*db.Queries
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("opening pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return &Store{Pool: pool, Queries: db.New(pool)}, nil
}

// Close releases all connections.
func (s *Store) Close() { s.Pool.Close() }

// InTx runs fn inside a transaction. It commits if fn returns nil and rolls
// back otherwise, including on panic.
func (s *Store) InTx(ctx context.Context, fn func(q *db.Queries) error) (err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx) // the panic is the error that matters
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("rolling back: %w", rbErr))
			}
		}
	}()
	if err = fn(db.New(tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

// IsNotFound reports whether err means a query returned no rows.
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
