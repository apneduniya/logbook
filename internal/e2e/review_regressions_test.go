package e2e

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Cross-model review of ADR 0017, finding 2: the agent before migration
// 0007 recorded an upload the server refused as final exactly like an
// accepted one. Its stored 60 s was never the server's value (30 s); after
// the migration the agent must end up holding what the server holds.
func TestLegacyRefusedValueReconciledAfterMigration(t *testing.T) {
	ctx := context.Background()
	legacy := legacyDB("2026-11-19")
	r := newRig(t, m(31).Add(5*time.Second), registeredOn(m(-24*60)), beforeOpen(func(r *rig) {
		legacy(r) //nolint:contextcheck // test fixture: builds a migration-6 database on disk
		raw, err := sql.Open("sqlite", r.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		if _, err := raw.ExecContext(ctx, `INSERT INTO credited_minutes
			(minute, project, label, credited, cuts, flags, rules_version, digest, sent_digest, sent_final, cutoff)
			VALUES (?, 'app', 'you', 60, '[]', '[]', 'v1', 'sixty', 'sixty', 1, 0)`, m(0).Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := r.st.Pool.Exec(ctx, `INSERT INTO credited_minutes
			(device_id, user_id, minute, project, label, credited, cuts, flags, rules_version, updated_at)
			VALUES ($1, $2, $3, 'app', 'you', 30, '{}', '{}', 'v1', $3)`, r.device, r.user, m(0)); err != nil {
			t.Fatal(err)
		}
	}))
	r.run()
	local := r.mustStored(m(0))
	_, held := r.serverRows(m(0))
	if len(local) != 1 || !local[0].Final || !local[0].Acknowledged || local[0].Credited != held || held != 30 {
		t.Fatalf("after migrating: local %+v, server holds %d s; want both 30 s, final and acknowledged", local, held)
	}
}

// Cross-model review of ADR 0017, finding 1: the row cap deleted sealed
// rows that a still-open minute read, so its final value no longer matched
// a replay at its key from the sealed blocks. The cap now keeps rows newer
// than localdb.CapHorizon, and finish() replays every minute from blocks.
func TestRowCapKeepsInputsOfOpenMinutes(t *testing.T) {
	ctx := context.Background()
	at := func(h, mi int) time.Time { return time.Date(2026, 11, 20, h, mi, 0, 0, time.UTC) }
	r := newRig(t, at(24, 41).Add(5*time.Second), registeredOn(at(0, 0)))
	r.present(at(24, 35), at(25, 1))
	r.store(ai(at(24, 40).Add(5*time.Second), wire.AIToolStart), ai(at(24, 49), wire.AITurnEnd))
	r.runEvery(at(24, 41), at(25, 1), nil)
	target := at(24, 40)
	if r.storedCredit(target) != 60 {
		t.Fatalf("setup did not credit the target: %+v", r.mustStored(target))
	}
	if len(r.sealedDays()) != 1 {
		t.Fatal("the previous day was not sealed")
	}
	// The smallest cap: every sealed row is a candidate for trimming.
	if err := r.db.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := localdb.Open(ctx, r.dbPath, localdb.WithClock(r.clock), localdb.WithEventCap(1))
	if err != nil {
		t.Fatal(err)
	}
	r.db, r.opts.Store = d, d
	t.Cleanup(func() { _ = d.Close() })
	r.restart()
	r.set(at(25, 2).Add(5 * time.Second))
	r.present(at(25, 1), at(25, 1).Add(5*time.Second))
	// AI events are always kept, so the next block has a row to seal.
	r.store(ai(at(25, 1).Add(10*time.Second), wire.AIToolStart))
	r.pruned = true
	r.run()
	if got := r.storedCredit(target); got != 60 {
		t.Fatalf("target minute credit after the cap = %d, want 60", got)
	}
	r.finish("2026-11-21")
}
