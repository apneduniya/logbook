package crediting

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// A sealed block must rebuild to the same bytes later, whatever is logged
// after sealing: late active-time points (OTLP export interval, delta
// temporality), late heartbeats (wakatime-cli's offline queue) and late
// hook events, which also change derived values such as a session's
// working directory. Those go into the next block.
func TestSealedBlockRebuildsIdentically(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	e.options = func(o *Options) { o.ActiveTimes = e.db }
	e.open()
	day := "2026-11-19"
	start, _, _ := evidence.DayBounds(day)
	addDay(t, e.db, day)
	if err := e.db.MarkReceiverUp(ctx, start.Add(8*time.Hour), start.Add(13*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "ai", AI: &wire.AIEvent{
		Time: start.Add(9 * time.Hour), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit,
		SessionID: "cc", Cwd: "/src/app"}}}); err != nil {
		t.Fatal(err)
	}
	point := func(from time.Time, secs float64) localdb.ActiveTimePoint {
		return localdb.ActiveTimePoint{From: from, To: from.Add(time.Minute), SessionID: "cc", Type: localdb.ActiveUser, Secs: secs}
	}
	if err := e.db.InsertActiveTime(ctx, []localdb.ActiveTimePoint{point(start.Add(10*time.Hour), 30)}); err != nil {
		t.Fatal(err)
	}
	e.run() // registers the device

	e.set(time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC))
	e.run()
	id := e.deviceID()
	days := e.srv.storedDays(id)
	if len(days) != 1 || days[0].EventCounts[evidence.TypeActiveTime] != 1 || days[0].EventCounts[evidence.TypeAI] != 2 {
		t.Fatalf("server days = %+v, want the 19th with one active-time point and two AI events", days)
	}

	// Late arrivals for the sealed day.
	if err := e.db.InsertActiveTime(ctx, []localdb.ActiveTimePoint{point(start.Add(11*time.Hour), 20)}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "ai", AI: &wire.AIEvent{
		Time: start.Add(9*time.Hour + 30*time.Minute), Harness: wire.HarnessClaudeCode, Kind: wire.AIToolStart,
		SessionID: "cc", Cwd: "/src/other"}}}); err != nil {
		t.Fatal(err)
	}
	addDay(t, e.db, day)

	sealed, ok, err := e.db.LastEvidenceDay(ctx, id)
	if err != nil || !ok {
		t.Fatalf("LastEvidenceDay = %v, %v", ok, err)
	}
	rows, err := ReplayBlock(ctx, e.db, sealed)
	if err != nil {
		t.Fatal(err)
	}
	got := rehash(t, rows)
	if got != sealed.ContentHash || got != days[0].ContentHash {
		t.Fatalf("rebuilt content hash = %s, sealed %s", got, sealed.ContentHash)
	}

	// A block whose rows are no longer all there says so instead of
	// hashing something else, and so does a day sealed before the log.
	short := sealed
	short.SeqTo--
	if _, err := ReplayBlock(ctx, e.db, short); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("replay with missing rows: %v", err)
	}
	legacy := sealed
	legacy.Legacy = true
	if _, err := ReplayBlock(ctx, e.db, legacy); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("replay of a legacy day: %v", err)
	}
}

// ReplayMinute runs the engine at a minute's key: rows logged after the key
// (later events, a changed repo mapping) don't change it.
func TestReplayMinuteUsesTheKey(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, t0)
	e.options = func(o *Options) { o.Compute = struggle.Compute }
	e.open()
	sample := func(at time.Time) wire.LocalEvent {
		return wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{Time: at, FocusKind: "editor", FocusApp: "Code"}}
	}
	var evs []wire.LocalEvent
	for s := at("11:50"); s.Before(at("11:52")); s = s.Add(5 * time.Second) {
		evs = append(evs, sample(s))
	}
	if err := e.db.InsertEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	hb := wire.Heartbeat{Entity: "/src/app/main.go", Time: float64(at("11:50").Add(30 * time.Second).Unix()), Project: "app", IsWrite: true}
	if err := e.db.Enqueue(ctx, []wire.Heartbeat{hb}, false); err != nil {
		t.Fatal(err)
	}
	e.run()
	stored, err := e.db.StoredMinutes(ctx, at("11:50"), at("11:51"))
	if err != nil || len(stored) == 0 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	key := stored[0].Key
	// Later rows for the same minute, and the mapping renames the project.
	if err := e.db.Enqueue(ctx, []wire.Heartbeat{{Entity: "/src/lib/x.go", Time: float64(at("11:50").Add(40 * time.Second).Unix()), Project: "lib"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := e.db.RecordRepos(ctx, t0, []struggle.Repo{{Path: "/src/app", Project: "renamed"}}); err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayMinute(ctx, e.db, at("11:50"), key, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != len(stored) {
		t.Fatalf("replayed %+v, stored %+v", replayed, stored)
	}
	for i := range stored {
		if creditOf(replayed[i]) != creditOf(stored[i]) {
			t.Fatalf("replayed %s, stored %s", creditOf(replayed[i]), creditOf(stored[i]))
		}
	}
	if _, err := ReplayMinute(ctx, e.db, at("11:50"), localdb.Key{}, nil, nil); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("replay of an unkeyed minute: %v", err)
	}
	ahead := key
	ahead.H += 1000
	if _, err := ReplayMinute(ctx, e.db, at("11:50"), ahead, nil, nil); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("replay past the log: %v", err)
	}
}

// repoHook is a repo source that calls a function before the run's
// snapshot is taken.
type repoHook func()

func (h repoHook) ActiveRepos(context.Context) ([]struggle.Repo, error) { h(); return nil, nil }

// A write that hasn't committed when a run takes its snapshot is entirely
// outside the run: its row is not in the engine's input and its sequence
// number is not in the run's key, even though it commits before the run
// stores its result. The next run sees it.
func TestUncommittedWriteIsOutsideTheRun(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, t0)
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity",
		Activity: &wire.ActivitySample{Time: at("11:50"), FocusKind: "editor"}}}); err != nil {
		t.Fatal(err)
	}
	// Another process's writer, holding the write lock with an insert that
	// has taken sequence number 2.
	raw, err := sql.Open("sqlite", config.DBPath(e.dir)+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var began, committed bool
	e.options = func(o *Options) {
		o.Repos = repoHook(func() {
			if began {
				return
			}
			began = true
			exec("BEGIN IMMEDIATE")
			exec("INSERT INTO activity_samples (seq, time, idle_seconds, focus_app, focus_kind) VALUES (2, ?, 0, '', 'editor')",
				at("11:55").UnixMilli())
			exec("UPDATE kv SET value = '2' WHERE key = 'log.seq'")
		})
		o.Compute = func(in struggle.Input) struggle.Result {
			if began && !committed {
				committed = true
				exec("COMMIT") // after the snapshot, before the result is stored
			}
			return e.compute(in)
		}
	}
	e.open()
	e.credit(at("11:58"), "app", 60)
	e.run()
	if !committed {
		t.Fatal("the writer never committed")
	}
	if in := e.inputs[0]; len(in.Activity) != 1 {
		t.Fatalf("first run read %d samples, want only the committed one", len(in.Activity))
	}
	stored, err := e.db.StoredMinutes(ctx, at("11:58"), at("11:59"))
	if err != nil || len(stored) != 1 || stored[0].Key != (localdb.Key{T: at("12:00"), H: 1}) {
		t.Fatalf("first run stored %+v, %v; want key (12:00, 1)", stored, err)
	}
	e.credit(at("11:58"), "app", 30)
	e.advance(time.Minute)
	e.run()
	if in := e.inputs[1]; len(in.Activity) != 2 {
		t.Fatalf("next run read %d samples, want both", len(in.Activity))
	}
	if stored, _ := e.db.StoredMinutes(ctx, at("11:58"), at("11:59")); stored[0].Key != (localdb.Key{T: at("12:01"), H: 2}) {
		t.Fatalf("next run stored %+v, want key (12:01, 2)", stored)
	}
}
