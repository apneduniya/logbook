package localdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
)

// RepoMapping is one row of the repo mapping journal: from Since on, files
// and working directories under Path belong to Project.
type RepoMapping struct {
	Since         time.Time
	Path, Project string
}

// RecordRepos adds a journal row, dated at, for each repo whose path has no
// row yet or whose latest row names another project. Repos with an empty
// path or project are ignored. Rows are never changed, so the mapping in
// force at any earlier time stays readable.
func (d *DB) RecordRepos(ctx context.Context, at time.Time, repos []struggle.Repo) error {
	if len(repos) == 0 {
		return nil
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		for _, r := range repos {
			if r.Path == "" || r.Project == "" {
				continue
			}
			var latest string
			err := tx.QueryRowContext(ctx, qRepoLatest, r.Path).Scan(&latest)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("reading repo mapping: %w", err)
			}
			if err == nil && latest == r.Project {
				continue
			}
			if _, err := tx.ExecContext(ctx, qRepoInsert, at.UnixMilli(), r.Path, r.Project); err != nil {
				return fmt.Errorf("recording repo mapping: %w", err)
			}
		}
		return nil
	})
}

// RepoMap returns every journal row in the order they were added.
func (d *DB) RepoMap(ctx context.Context) ([]RepoMapping, error) {
	var out []RepoMapping
	err := d.query(ctx, qRepoAll, nil, func(r *sql.Rows) error {
		var (
			m     RepoMapping
			since int64
		)
		if err := r.Scan(&since, &m.Path, &m.Project); err != nil {
			return err
		}
		m.Since = time.UnixMilli(since).UTC()
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading repo mapping: %w", err)
	}
	return out, nil
}
