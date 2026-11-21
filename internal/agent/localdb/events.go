package localdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// KV is a key-value pair written in the same transaction as a batch of
// events, so a caller can record its progress atomically with the data.
type KV struct{ Key, Value string }

// InsertEvents stores a batch of local events in one transaction, together
// with any progress marks. Every event must already be valid
// (wire.LocalEvent.Validate); an invalid one fails the whole batch.
//
// Each stored row takes the next sequence number of the event log in this
// transaction (ADR 0017); a commit already recorded takes none. Tables over
// the row cap lose their oldest rows only if the evidence chain has sealed
// them. Activity samples that find the table full of unsealed rows are
// dropped and counted (KeyDroppedSamples) instead; AI and git events are
// always kept.
func (d *DB) InsertEvents(ctx context.Context, evs []wire.LocalEvent, marks ...KV) error {
	for i, e := range evs {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
	}
	var ai, act, git []wire.LocalEvent
	for _, e := range evs {
		switch e.Type {
		case "ai":
			ai = append(ai, e)
		case "activity":
			act = append(act, e)
		case "git":
			git = append(git, e)
		}
	}
	return d.inTx(ctx, func(tx *sql.Tx) error {
		now := d.now()
		w, err := openLog(ctx, tx)
		if err != nil {
			return err
		}
		sealed, err := sealedSeq(ctx, tx)
		if err != nil {
			return err
		}
		var trimmed int64
		if len(act) > 0 {
			n, err := d.insertActivity(ctx, tx, w, act, sealed, now)
			if err != nil {
				return err
			}
			trimmed += n
		}
		for _, batch := range []struct {
			evs         []wire.LocalEvent
			insert, cap string
		}{{ai, qAIInsert, qAITrim}, {git, qGitInsert, qGitTrim}} {
			if len(batch.evs) == 0 {
				continue
			}
			if err := insertEach(ctx, tx, w, batch.insert, batch.evs); err != nil {
				return err
			}
			n, err := trimSealed(ctx, tx, batch.cap, d.eventCap, sealed, now.Add(-CapHorizon))
			if err != nil {
				return err
			}
			trimmed += n
		}
		if err := w.close(ctx, tx, now); err != nil {
			return err
		}
		if err := bumpGeneration(ctx, tx, trimmed, now); err != nil {
			return err
		}
		for _, m := range marks {
			if _, err := tx.ExecContext(ctx, qKVSet, m.Key, m.Value, now.UnixMilli()); err != nil {
				return fmt.Errorf("setting %s: %w", m.Key, err)
			}
		}
		return nil
	})
}

// insertActivity makes room by trimming sealed samples, then logs the
// samples that fit under the cap and counts the rest as dropped. It
// returns how many rows it trimmed.
func (d *DB) insertActivity(ctx context.Context, tx *sql.Tx, w *logWriter, evs []wire.LocalEvent, sealed int64, now time.Time) (int64, error) {
	trimmed, err := trimSealed(ctx, tx, qActivityTrim, d.eventCap-int64(len(evs)), sealed, now.Add(-CapHorizon))
	if err != nil {
		return 0, err
	}
	var held int64
	if err := tx.QueryRowContext(ctx, qActivitySpan).Scan(&held); err != nil {
		return 0, fmt.Errorf("sizing activity samples: %w", err)
	}
	room := max(d.eventCap-held, 0)
	keep := evs[:min(int64(len(evs)), room)]
	if err := insertEach(ctx, tx, w, qActivityInsert, keep); err != nil {
		return 0, err
	}
	if dropped := int64(len(evs) - len(keep)); dropped > 0 {
		if err := kvAdd(ctx, tx, KeyDroppedSamples, dropped, now.UnixMilli()); err != nil {
			return 0, err
		}
		d.log.WarnContext(ctx, "activity samples dropped: the local log is full of rows not sealed yet", "dropped", dropped)
	}
	return trimmed, nil
}

// insertEach logs events of one type with one prepared statement.
func insertEach(ctx context.Context, tx *sql.Tx, w *logWriter, query string, evs []wire.LocalEvent) error {
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("preparing: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, e := range evs {
		if err := w.insert(ctx, stmt, eventArgs(e)...); err != nil {
			return fmt.Errorf("storing %s event: %w", e.Type, err)
		}
	}
	return nil
}

// eventArgs are an event's insert parameters after its sequence number.
func eventArgs(e wire.LocalEvent) []any {
	switch e.Type {
	case "ai":
		a := e.AI
		return []any{a.Time.UnixMilli(), string(a.Harness), string(a.Kind), a.SessionID, a.Project, a.Cwd,
			a.Model, a.Tool, a.PromptChars, a.Decision, a.PromptHash}
	case "activity":
		s := e.Activity
		return []any{s.Time.UnixMilli(), s.IdleSeconds, s.FocusApp, s.FocusKind}
	case "git":
		g := e.Git
		return []any{g.Time.UnixMilli(), g.Project, g.Kind, g.CommitHash, g.FilesChanged, g.Insertions, g.Deletions}
	}
	return nil // Validate admits no other type
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
