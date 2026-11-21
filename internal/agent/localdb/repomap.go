package localdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
)

// RecordRepos adds a repo mapping row, dated at, for each repo whose path
// has no row yet or whose latest row names another project. Repos with an
// empty path or project are ignored. Each row is logged: it takes the next
// sequence number (ADR 0017), and is never changed, so the mapping at any
// earlier high-water mark stays readable (Snapshot.Repos).
func (d *DB) RecordRepos(ctx context.Context, at time.Time, repos []struggle.Repo) error {
	if len(repos) == 0 {
		return nil
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		w, err := openLog(ctx, tx)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, qRepoInsert)
		if err != nil {
			return fmt.Errorf("preparing: %w", err)
		}
		defer func() { _ = stmt.Close() }()
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
			if _, err := w.insert(ctx, stmt, at.UnixMilli(), r.Path, r.Project); err != nil {
				return fmt.Errorf("recording repo mapping: %w", err)
			}
		}
		return w.close(ctx, tx, d.now())
	})
}
