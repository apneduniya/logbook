package localdb

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/apneduniya/logbook/pkg/wire"
)

// KV is a key-value pair written in the same transaction as a batch of
// events, so a caller can record its progress atomically with the data.
type KV struct{ Key, Value string }

// InsertEvents stores a batch of local events in one transaction, together
// with any progress marks. Every event must already be valid
// (wire.LocalEvent.Validate); an invalid one fails the whole batch.
func (d *DB) InsertEvents(ctx context.Context, evs []wire.LocalEvent, marks ...KV) error {
	for i, e := range evs {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
	}
	now := d.now().UnixMilli()
	return d.inTx(ctx, func(tx *sql.Tx) error {
		var ai, act, git int
		for _, e := range evs {
			var err error
			switch e.Type {
			case "ai":
				ai++
				err = insertAI(ctx, tx, *e.AI)
			case "activity":
				act++
				err = insertActivity(ctx, tx, *e.Activity)
			case "git":
				git++
				err = insertGit(ctx, tx, *e.Git)
			}
			if err != nil {
				return fmt.Errorf("storing %s event: %w", e.Type, err)
			}
		}
		for _, t := range []struct {
			n     int
			query string
		}{{ai, qAITrim}, {act, qActivityTrim}, {git, qGitTrim}} {
			if t.n == 0 {
				continue
			}
			if _, err := trim(ctx, tx, t.query, d.eventCap); err != nil {
				return err
			}
		}
		for _, m := range marks {
			if _, err := tx.ExecContext(ctx, qKVSet, m.Key, m.Value, now); err != nil {
				return fmt.Errorf("setting %s: %w", m.Key, err)
			}
		}
		return nil
	})
}

// RecordActivity stores one activity sample. It makes *DB usable as the
// sensors' sink.
func (d *DB) RecordActivity(ctx context.Context, s wire.ActivitySample) error {
	return d.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity", Activity: &s}})
}

// RecordGit stores one git event. Commits already recorded are ignored.
func (d *DB) RecordGit(ctx context.Context, g wire.GitEvent) error {
	return d.InsertEvents(ctx, []wire.LocalEvent{{Type: "git", Git: &g}})
}

func insertAI(ctx context.Context, tx *sql.Tx, e wire.AIEvent) error {
	_, err := tx.ExecContext(ctx, qAIInsert, e.Time.UnixMilli(), string(e.Harness), string(e.Kind),
		e.SessionID, e.Project, e.Cwd, e.Model, e.Tool, e.PromptChars, e.Decision)
	return err
}

func insertActivity(ctx context.Context, tx *sql.Tx, s wire.ActivitySample) error {
	_, err := tx.ExecContext(ctx, qActivityInsert, s.Time.UnixMilli(), s.IdleSeconds, s.FocusApp, s.FocusKind)
	return err
}

func insertGit(ctx context.Context, tx *sql.Tx, g wire.GitEvent) error {
	_, err := tx.ExecContext(ctx, qGitInsert, g.Time.UnixMilli(), g.Project, g.Kind, g.CommitHash,
		g.FilesChanged, g.Insertions, g.Deletions)
	return err
}
