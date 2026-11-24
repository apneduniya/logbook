package crediting

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// LogReader reads the local event log in consistent snapshots. *localdb.DB
// satisfies it.
type LogReader interface {
	View(ctx context.Context, fn func(*localdb.Snapshot) error) error
}

// ErrNotReproducible is returned by a replay when local data can no longer
// rebuild what was sealed or computed: rows pruned or trimmed since, or a
// day sealed before the event log existed.
var ErrNotReproducible = errors.New("can no longer be rebuilt from local data")

// ReplayMinute recomputes minute m at its computation key (ADR 0017): the
// engine over the window ending at key.T, with every logged event of
// [key.T - Window - History, key.T) at or below key.H and the repo mapping
// at key.H. It returns the engine's rows for m, sorted by project; a
// stored minute's rows (removals aside) must match them on label, project,
// credit and cuts.
//
// The input is built by pkg/replay, as for the live run and the server's
// re-check. Claude Code active time is not replayed: it feeds only flags,
// as does the time zone (tz, UTC when nil). compute is the engine
// (struggle.Compute when nil).
func ReplayMinute(ctx context.Context, log LogReader, m time.Time, key localdb.Key,
	compute func(struggle.Input) struggle.Result, tz *time.Location) ([]localdb.CreditedMinute, error) {
	if compute == nil {
		compute = struggle.Compute
	}
	if tz == nil {
		tz = time.UTC
	}
	if key.IsZero() {
		return nil, fmt.Errorf("minute %s has no key: %w", m.UTC().Format(time.RFC3339), ErrNotReproducible)
	}
	s := spanAt(key.T)
	if !s.to.Equal(key.T.UTC()) {
		return nil, fmt.Errorf("key time %s is not a whole minute", key.T.Format(time.RFC3339Nano))
	}
	var (
		ev    localdb.Events
		repos []struggle.Repo
	)
	err := log.View(ctx, func(snap *localdb.Snapshot) error {
		if snap.H < key.H {
			return fmt.Errorf("log at %d is behind the key's %d: %w", snap.H, key.H, ErrNotReproducible)
		}
		var err error
		if ev, err = snap.Events(ctx, s.history, s.to, 0, key.H); err != nil {
			return err
		}
		repos, err = snap.Repos(ctx, key.H)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reading the log to replay %s: %w", m.UTC().Format(time.RFC3339), err)
	}
	in, err := engineInput(key, ev, nil, repos, tz)
	if err != nil {
		return nil, err
	}
	var out []localdb.CreditedMinute
	for _, row := range minuteRows(s, compute(in)) {
		if row.Minute.Equal(m.UTC()) {
			out = append(out, row)
		}
	}
	return out, nil
}

// ReplayBlock rebuilds the rows a sealed evidence block covers: every
// logged row with a sequence number in [e.SeqFrom, e.SeqTo]. Rows are never
// updated, so hashing the result with the block's bucket salts
// (evidence.HashBlock) gives the sealed content hash. It returns ErrNotReproducible when local data no longer
// holds the block (rows deleted since, checked against the sealed counts)
// or the day was sealed before the event log (legacy).
func ReplayBlock(ctx context.Context, log LogReader, e localdb.EvidenceDay) ([]evidence.Row, error) {
	if e.Legacy {
		return nil, fmt.Errorf("%s was sealed before the event log: %w", e.Day, ErrNotReproducible)
	}
	var rows []evidence.Row
	err := log.View(ctx, func(s *localdb.Snapshot) error {
		var err error
		rows, err = s.Rows(ctx, e.SeqFrom-1, e.SeqTo)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reading the log to replay %s: %w", e.Day, err)
	}
	if got := evidence.Counts(rows); !maps.Equal(got, e.EventCounts) {
		return nil, fmt.Errorf("%s: row counts %v, sealed %v: %w", e.Day, got, e.EventCounts, ErrNotReproducible)
	}
	return rows, nil
}
