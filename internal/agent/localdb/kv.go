package localdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Get returns the value stored under key, or "" if there is none.
func (d *DB) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, qKVGet, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", key, err)
	}
	return v, nil
}

// Set stores value under key.
func (d *DB) Set(ctx context.Context, key, value string) error {
	if _, err := d.sql.ExecContext(ctx, qKVSet, key, value, d.now().UnixMilli()); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	return nil
}

// SetMany stores several values in one transaction.
func (d *DB) SetMany(ctx context.Context, kvs ...KV) error {
	now := d.now().UnixMilli()
	return d.inTx(ctx, func(tx *sql.Tx) error {
		for _, kv := range kvs {
			if _, err := tx.ExecContext(ctx, qKVSet, kv.Key, kv.Value, now); err != nil {
				return fmt.Errorf("writing %s: %w", kv.Key, err)
			}
		}
		return nil
	})
}

// Add increments the integer counter under key by n.
func (d *DB) Add(ctx context.Context, key string, n int64) error {
	return d.inTx(ctx, func(tx *sql.Tx) error { return kvAdd(ctx, tx, key, n, d.now().UnixMilli()) })
}

// Counter returns the integer counter under key (0 if unset).
func (d *DB) Counter(ctx context.Context, key string) (int64, error) {
	v, err := d.Get(ctx, key)
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("counter %s is not a number: %w", key, err)
	}
	return n, nil
}

// Prefix returns every key-value pair whose key starts with prefix.
func (d *DB) Prefix(ctx context.Context, prefix string) (map[string]string, error) {
	// Keys in [prefix, prefix+"\xff") share the prefix; agent keys are ASCII.
	rows, err := d.sql.QueryContext(ctx, qKVPrefix, prefix, prefix+"\xff")
	if err != nil {
		return nil, fmt.Errorf("reading %s*: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scanning kv: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s*: %w", prefix, err)
	}
	return out, nil
}

func kvAdd(ctx context.Context, tx *sql.Tx, key string, n, now int64) error {
	if _, err := tx.ExecContext(ctx, qKVAdd, key, n, now); err != nil {
		return fmt.Errorf("incrementing %s: %w", key, err)
	}
	return nil
}
