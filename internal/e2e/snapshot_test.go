package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// rawConn opens a second connection to the agent's database, as another
// process (a hook, the CLI) has, and returns an exec helper bound to it.
func (r *rig) rawConn() func(q string, args ...any) {
	r.t.Helper()
	ctx := context.Background()
	raw, err := sql.Open("sqlite", r.dbPath+"?_busy_timeout=5000")
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = raw.Close() })
	conn, err := raw.Conn(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	return func(q string, args ...any) {
		r.t.Helper()
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			r.t.Fatalf("%s: %v", q, err)
		}
	}
}

// Round-4 counterexample 1: a row inserted by a concurrent writer that is
// uncommitted when the run reads. A device without an input sensor reads
// app's main.go every minute, so 12:00 is idle. The run at 12:29 makes
// 12:00 final. While it takes its snapshot, another process has logged a
// save at 12:00:40 but not committed it; the save commits after the
// snapshot, before the run stores its result.
//
// Before ADR 0017 the save's arrival stamp was before the run's cutoff, so
// a replay with that cutoff included a row the run never read and
// disagreed. Now the row's sequence number is above the run's snapshot H:
// the run, its key and any replay at that key leave it out, and the next
// run counts it as late.
func TestUncommittedWriteOutsideFinalRun(t *testing.T) {
	t.Parallel()
	r := newRig(t, m(-12).Add(5*time.Second))
	r.reading(m(-12), m(29))
	exec := r.rawConn()
	save := appHeartbeat(m(0).Add(40*time.Second), true)
	payload, err := json.Marshal(save)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	r.with(func(rr *rig) {
		rr.beforeSnapshot = func() {
			seq = r.high() + 1
			exec("BEGIN IMMEDIATE")
			exec("INSERT INTO heartbeats (seq, time, fields_hash, payload) VALUES (?, ?, ?, ?)",
				seq, save.Timestamp().UnixMilli(), save.FieldsHash(), payload)
			exec("UPDATE kv SET value = ? WHERE key = 'log.seq'", fmt.Sprint(seq))
		}
		rr.afterSnapshot = func() { exec("COMMIT") }
	})
	r.reading(m(29), m(30))
	if r.high() != seq {
		t.Fatalf("log at %d after the run, want the concurrent save (%d) committed", r.high(), seq)
	}
	rows := r.mustStored(m(0))
	if len(rows) != 1 || !rows[0].Final || rows[0].Credited != 0 || rows[0].Key.H >= seq {
		t.Fatalf("12:00 = %+v, want final and idle at a key before row %d", rows, seq)
	}
	if held := r.serverMinutes(m(0), m(1)); len(held) != 1 || held[0].text() != localText(rows[0]) {
		t.Fatalf("server 12:00 = %+v, agent %+v", held, rows)
	}

	r.reading(m(30), m(33))
	if got := r.stored(m(0), m(1)); len(got) != 1 || got[0] != localText(rows[0]) {
		t.Fatalf("12:00 changed after it was final: %v", got)
	}
	if s := r.status(); s.Minutes.LateIgnored != 1 {
		t.Fatalf("late events = %d, want the concurrent save", s.Minutes.LateIgnored)
	}
	r.finish("2026-11-20")
}

// Round-4 counterexample 6: the repo mapping changes between the run that
// makes a minute final (T = m+29) and the next. The person saves
// /src/mono/svc/main.go every minute; the editor names the project "mono",
// and so does the repo mapping until, just before the run at 12:30, the
// repo source reports /src/mono/svc as its own repo "svc".
//
// Before ADR 0017 repo mappings were grouped by time with a +30 minute
// boundary while finality used +29, so a replay could apply a different
// mapping than the run that made the minute final. Now each run reads one
// mapping, at its snapshot: 12:00 (final at 12:29) stays "mono", 12:01
// (final at 12:30) is "svc", and each replays with the mapping at its key.
func TestRepoMappingChangeAtFinality(t *testing.T) {
	t.Parallel()
	r := newRig(t, m(1).Add(5*time.Second))
	r.with(func(r *rig) { r.repos = []struggle.Repo{{Path: "/src/mono", Project: "mono"}} })
	work := func(now time.Time) {
		prev := now.Truncate(time.Minute).Add(-time.Minute)
		r.present(prev, prev.Add(time.Minute))
		r.enqueue(wire.Heartbeat{Entity: "/src/mono/svc/main.go", Project: "mono", IsWrite: true,
			Time: float64(prev.Add(20*time.Second).UnixMilli()) / 1000})
	}
	r.runEvery(m(1), m(30), work)
	final := r.mustStored(m(0))
	if len(final) != 1 || final[0].Project != "mono" || !final[0].Final {
		t.Fatalf("12:00 at 12:29 = %+v, want final under mono", final)
	}

	r.with(func(r *rig) {
		r.repos = append(r.repos, struggle.Repo{Path: "/src/mono/svc", Project: "svc"})
	})
	r.set(m(30).Add(5 * time.Second))
	work(r.clock())
	mapping := r.high() + 1 // the run logs the new mapping row first
	r.run()
	r.runEvery(m(31), m(33), work)

	if got := r.mustStored(m(0)); len(got) != 1 || localText(got[0]) != localText(final[0]) || got[0].Key.H >= mapping {
		t.Fatalf("12:00 = %+v after the mapping changed, want %+v", got, final)
	}
	next := r.mustStored(m(1))
	if len(next) != 1 || next[0].Project != "svc" || !next[0].Key.T.Equal(m(30)) || next[0].Key.H < mapping {
		t.Fatalf("12:01 = %+v, want svc at the 12:30 run's key (mapping row %d)", next, mapping)
	}
	r.finish("2026-11-20")
}

// legacyDB builds, at r.dbPath, the database of an agent from before the
// event log (local migration 6) that sealed day by event time, with a
// record the server accepted, and logged three activity samples that
// evening.
func legacyDB(day string) func(r *rig) {
	return func(r *rig) {
		r.t.Helper()
		ctx := context.Background()
		files, err := filepath.Glob("../agent/localdb/migrations/000[1-6]_*.sql")
		if err != nil || len(files) != 6 {
			r.t.Fatalf("migrations 1-6: %v, %v", files, err)
		}
		raw, err := sql.Open("sqlite", r.dbPath)
		if err != nil {
			r.t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		exec := func(q string, args ...any) {
			r.t.Helper()
			if _, err := raw.ExecContext(ctx, q, args...); err != nil {
				r.t.Fatalf("%.60s: %v", q, err)
			}
		}
		exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
		for i, f := range files {
			b, err := os.ReadFile(f) //nolint:gosec // the repository's own migrations
			if err != nil {
				r.t.Fatal(err)
			}
			exec(string(b))
			exec(`INSERT INTO schema_migrations VALUES (?, 0)`, i+1)
		}

		d, err := time.Parse(time.DateOnly, day)
		if err != nil {
			r.t.Fatal(err)
		}
		for i := range 3 {
			at := d.Add(22*time.Hour + time.Duration(i)*10*time.Second).UnixMilli()
			exec(`INSERT INTO activity_samples (time, idle_seconds, focus_kind, arrived_at) VALUES (?, 1, 'editor', ?)`, at, at)
		}
		e := evidence.Day{DeviceID: r.device.String(), Day: day, ContentHash: strings.Repeat("ab", 32), RulesVersion: struggle.RulesVersion}
		chain, sig, err := r.key.Sign(e)
		if err != nil {
			r.t.Fatal(err)
		}
		counts := map[string]int{evidence.TypeActivity: 3}
		if err := r.client.UploadEvidenceDay(ctx, r.target, e.DeviceID, serverapi.EvidenceDayRequest{
			Day: day, ContentHash: e.ContentHash, ChainHash: chain, Signature: sig, RulesVersion: e.RulesVersion, EventCounts: counts,
		}); err != nil {
			r.t.Fatal(err)
		}
		exec(`INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version,
			event_counts, hw_heartbeats, hw_ai, hw_activity, hw_git, state, created_at, updated_at)
			VALUES (?, ?, '', ?, ?, ?, ?, '{"activity":3}', 0, 0, 3, 0, 'accepted', 0, 0)`,
			e.DeviceID, day, e.ContentHash, chain, sig, e.RulesVersion)
	}
}

// Round-4 counterexample 9: days sealed before local migration 0007 are
// legacy. They covered events by event time, which the log can no longer
// reproduce, so a replay must refuse them (ErrNotReproducible) rather than
// hash the log and report a mismatch, or a match by accident. The chain
// goes on from the legacy day, and the next block starts at the oldest
// retained row, so the evening's samples are sealed again by sequence
// number.
func TestLegacyDayRefusedForReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, m(1).Add(5*time.Second), registeredOn(m(-24*60)), beforeOpen(legacyDB("2026-11-19")))
	legacy, ok, err := r.db.EvidenceDayOf(ctx, r.device.String(), "2026-11-19")
	if err != nil || !ok || !legacy.Legacy {
		t.Fatalf("Nov 19 after the migration = %+v, %v, %v; want legacy", legacy, ok, err)
	}
	if _, err := crediting.ReplayBlock(ctx, r.db, legacy); !errors.Is(err, crediting.ErrNotReproducible) {
		t.Fatalf("replaying the legacy day: %v, want ErrNotReproducible", err)
	}

	r.present(m(-5), m(10))
	r.edit(m(0).Add(20*time.Second), "app")
	r.runEvery(m(1), m(3), nil)
	r.finish("2026-11-20")
	nov20, _, err := r.db.EvidenceDayOf(ctx, r.device.String(), "2026-11-20")
	if err != nil {
		t.Fatal(err)
	}
	if nov20.SeqFrom != 1 || nov20.PrevChainHash != legacy.ChainHash || nov20.EventCounts[evidence.TypeActivity] < 3 {
		t.Fatalf("Nov 20 = %+v, want a block from row 1 chained to the legacy day", nov20)
	}
}

// busySession logs [from, to) of a busy session the way the benchmarks in
// internal/agent/crediting do: a 5 s activity sample, a save every 15 s and
// an AI tool call every 20 s. It returns how many samples it logged.
func busySession(r *rig, from, to time.Time) int {
	r.t.Helper()
	var evs []wire.LocalEvent
	var hbs []wire.Heartbeat
	n := 0
	for ts := from; ts.Before(to); ts = ts.Add(5 * time.Second) {
		evs = append(evs, wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{Time: ts, FocusKind: "editor", FocusApp: "Code"}})
		n++
		if ts.Sub(from)%(15*time.Second) == 0 {
			hbs = append(hbs, wire.Heartbeat{Entity: "/src/app/main.go", Project: "app", IsWrite: true, Time: float64(ts.UnixMilli()) / 1000})
		}
		if ts.Sub(from)%(20*time.Second) == 0 {
			evs = append(evs, wire.LocalEvent{Type: "ai", AI: &wire.AIEvent{Time: ts, Harness: wire.HarnessClaudeCode,
				Kind: wire.AIToolStart, SessionID: "s", Cwd: "/src/app"}})
		}
	}
	r.store(evs...)
	r.enqueue(hbs...)
	return n
}

// Round-4 counterexample 10: the crediting benchmark's fixture must be in
// the run it measures. Before ADR 0017 the fixture's arrival stamps fell
// after the first run's cutoff, so the benchmark timed an engine that saw
// no events. Logged rows are in every later snapshot, so the first run
// reads the whole two hours and uploads its minutes.
func TestBenchmarkFixtureIsRead(t *testing.T) {
	t.Parallel()
	r := newRig(t, minute.Add(5*time.Second))
	n := busySession(r, minute.Add(-2*time.Hour), minute)
	r.run()
	if got := len(r.input.Activity); got != n {
		t.Fatalf("the first run read %d samples, want the fixture's %d", got, n)
	}
	if len(r.input.Heartbeats) == 0 || len(r.input.AI) == 0 {
		t.Fatalf("the first run read %d heartbeats and %d AI events", len(r.input.Heartbeats), len(r.input.AI))
	}
	if held := r.serverMinutes(m(-60), m(0)); len(held) < 60 {
		t.Fatalf("the server holds %d rows for the fixture's last hour, want at least 60", len(held))
	}
	r.finish("2026-11-20")
}

// BenchmarkRunOnce times one minute of the whole pipeline in steady state:
// log a busy minute, compute, store and upload to the real server. The
// first run must read the fixture (TestBenchmarkFixtureIsRead).
func BenchmarkRunOnce(b *testing.B) {
	r := newRig(b, minute.Add(5*time.Second))
	n := busySession(r, minute.Add(-2*time.Hour), minute)
	r.runAny()
	if got := len(r.input.Activity); got != n {
		b.Fatalf("the first run read %d samples, want the fixture's %d", got, n)
	}
	now := minute
	for b.Loop() {
		busySession(r, now, now.Add(time.Minute))
		now = now.Add(time.Minute)
		r.set(now.Add(5 * time.Second))
		r.runAny()
	}
}
