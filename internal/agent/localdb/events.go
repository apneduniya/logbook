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
//
// Each row records its arrival: the clock when the transaction holds the
// write lock, so a row stamped before a reader's cutoff is committed before
// that reader starts (ADR 0016).
func (d *DB) InsertEvents(ctx context.Context, evs []wire.LocalEvent, marks ...KV) error {
	for i, e := range evs {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		now := d.now()
		arrived := now.UnixMilli()
		var ai, act, git int
		for _, e := range evs {
			var err error
			switch e.Type {
			case "ai":
				ai++
				err = insertAI(ctx, tx, *e.AI, arrived)
			case "activity":
				act++
				err = insertActivity(ctx, tx, *e.Activity, arrived)
			case "git":
				git++
				err = insertGit(ctx, tx, *e.Git, arrived)
			}
			if err != nil {
				return fmt.Errorf("storing %s event: %w", e.Type, err)
			}
		}
		var trimmed int64
		for _, t := range []struct {
			n     int
			query string
		}{{ai, qAITrim}, {act, qActivityTrim}, {git, qGitTrim}} {
			if t.n == 0 {
				continue
			}
			n, err := trim(ctx, tx, t.query, d.eventCap)
			if err != nil {
				return err
			}
			trimmed += n
		}
		if err := bumpGeneration(ctx, tx, trimmed, now); err != nil {
			return err
		}
		for _, m := range marks {
			if _, err := tx.ExecContext(ctx, qKVSet, m.Key, m.Value, arrived); err != nil {
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

func insertAI(ctx context.Context, tx *sql.Tx, e wire.AIEvent, arrived int64) error {
	_, err := tx.ExecContext(ctx, qAIInsert, e.Time.UnixMilli(), string(e.Harness), string(e.Kind),
		e.SessionID, e.Project, e.Cwd, e.Model, e.Tool, e.PromptChars, e.Decision, e.PromptHash, arrived)
	return err
}

func insertActivity(ctx context.Context, tx *sql.Tx, s wire.ActivitySample, arrived int64) error {
	_, err := tx.ExecContext(ctx, qActivityInsert, s.Time.UnixMilli(), s.IdleSeconds, s.FocusApp, s.FocusKind, arrived)
	return err
}

func insertGit(ctx context.Context, tx *sql.Tx, g wire.GitEvent, arrived int64) error {
	_, err := tx.ExecContext(ctx, qGitInsert, g.Time.UnixMilli(), g.Project, g.Kind, g.CommitHash,
		g.FilesChanged, g.Insertions, g.Deletions, arrived)
	return err
}
