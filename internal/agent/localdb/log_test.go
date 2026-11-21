package localdb

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// seal marks every row up to seq as sealed by the device's chain.
func seal(t *testing.T, d *DB, seq int64) {
	t.Helper()
	if err := d.SetMany(context.Background(), KV{KeySealed, strconv.FormatInt(seq, 10)}); err != nil {
		t.Fatal(err)
	}
}

// high returns the log's current high-water mark.
func high(ctx context.Context, t *testing.T, d *DB) int64 {
	t.Helper()
	var h int64
	if err := d.View(ctx, func(s *Snapshot) error { h = s.H; return nil }); err != nil {
		t.Fatal(err)
	}
	return h
}

func activity(at time.Time) wire.LocalEvent {
	return wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{Time: at, IdleSeconds: 1, FocusKind: "editor"}}
}

// Every logged insert takes the next sequence number from one counter,
// across tables; ignored duplicates take none.
func TestLogSequenceNumbers(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	commit := wire.GitEvent{Time: minute(-5), Project: "app", Kind: "commit", CommitHash: "abc"}
	steps := []struct {
		name  string
		store func() error
		want  int64
	}{
		{"activity", func() error { return d.RecordActivity(ctx, *activity(minute(-5)).Activity) }, 1},
		{"two heartbeats", func() error { return d.Enqueue(ctx, []wire.Heartbeat{hb(1), hb(2)}, false) }, 3},
		{"a resent heartbeat", func() error { return d.Enqueue(ctx, []wire.Heartbeat{hb(1)}, false) }, 3},
		{"a commit", func() error { return d.RecordGit(ctx, commit) }, 4},
		{"the same commit", func() error { return d.RecordGit(ctx, commit) }, 4},
		{"active time", func() error {
			return d.InsertActiveTime(ctx, []ActiveTimePoint{{From: minute(-2), To: minute(-1), SessionID: "s", Type: ActiveUser, Secs: 3}})
		}, 5},
		{"a retried export", func() error {
			return d.InsertActiveTime(ctx, []ActiveTimePoint{{From: minute(-2), To: minute(-1), SessionID: "s", Type: ActiveUser, Secs: 3}})
		}, 5},
		{"a repo", func() error { return d.RecordRepos(ctx, testNow, []struggle.Repo{{Path: "/src/app", Project: "app"}}) }, 6},
		{"the same repo", func() error { return d.RecordRepos(ctx, testNow, []struggle.Repo{{Path: "/src/app", Project: "app"}}) }, 6},
	}
	for _, s := range steps {
		if err := s.store(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if h := high(ctx, t, d); h != s.want {
			t.Fatalf("after %s: H = %d, want %d", s.name, h, s.want)
		}
	}
	// The committed rows are exactly 1..H.
	var rows []evidence.Row
	if err := d.View(ctx, func(s *Snapshot) error {
		var err error
		rows, err = s.Rows(ctx, 0, s.H)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, r := range rows {
		seqs = append(seqs, r.Seq)
	}
	slices.Sort(seqs)
	if !slices.Equal(seqs, []int64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("logged rows = %v, want 1..6", seqs)
	}
	if c := evidence.Counts(rows); c[evidence.TypeHeartbeat] != 2 || c[evidence.TypeRepo] != 1 || c[evidence.TypeActiveTime] != 1 {
		t.Fatalf("row counts = %v", c)
	}
}

// A snapshot reads rows at or below its high-water mark, so a window read
// at an older mark gives exactly the rows logged by then.
func TestReadWindowAtHighWater(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	at := minute(-30)
	evs := []wire.LocalEvent{
		{Type: "ai", AI: &wire.AIEvent{Time: at, Harness: wire.HarnessCodex, Kind: wire.AIPromptSubmit, SessionID: "s",
			Project: "app", Cwd: "/src/app", Model: "m", Tool: "t", PromptChars: 5, Decision: "allow", PromptHash: "ab"}},
		{Type: "activity", Activity: &wire.ActivitySample{Time: at, IdleSeconds: 2.5, FocusApp: "Code", FocusKind: "editor"}},
		{Type: "git", Git: &wire.GitEvent{Time: at, Project: "app", Kind: "worktree", FilesChanged: 1, Insertions: 2, Deletions: 3}},
		activity(testNow), // at `to`: excluded
	}
	if err := d.InsertEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(ctx, []wire.Heartbeat{hb(1)}, false); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRepos(ctx, testNow, []struggle.Repo{{Path: "/src/app", Project: "old"}}); err != nil {
		t.Fatal(err)
	}
	h := high(ctx, t, d)
	// Rows logged later, for the same times, and a changed mapping.
	if err := d.InsertEvents(ctx, evs[:3]); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(ctx, []wire.Heartbeat{hb(3)}, false); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRepos(ctx, testNow, []struggle.Repo{{Path: "/src/app", Project: "new"}}); err != nil {
		t.Fatal(err)
	}
	from := testNow.Add(-time.Hour)
	pinned, err := d.ReadWindow(ctx, from, testNow, h)
	if err != nil {
		t.Fatal(err)
	}
	ev := pinned.Events
	if pinned.H != h || len(ev.AI) != 1 || len(ev.Activity) != 1 || len(ev.Git) != 1 || len(ev.Heartbeats) != 1 {
		t.Fatalf("window at H=%d: %+v", h, pinned)
	}
	if got := ev.AI[0].Event; got != *evs[0].AI {
		t.Fatalf("ai event round trip = %+v, want %+v", got, *evs[0].AI)
	}
	if got := ev.Activity[0].Event; got != *evs[1].Activity {
		t.Fatalf("activity round trip = %+v", got)
	}
	if got := ev.Git[0].Event; got != *evs[2].Git {
		t.Fatalf("git round trip = %+v", got)
	}
	if !slices.Equal(pinned.Repos, []struggle.Repo{{Path: "/src/app", Project: "old"}}) {
		t.Fatalf("repos at H = %+v", pinned.Repos)
	}
	now, err := d.ReadWindow(ctx, from, testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if now.H != h+5 || len(now.Events.AI) != 2 || len(now.Events.Activity) != 2 || len(now.Events.Heartbeats) != 2 ||
		now.Repos[0].Project != "new" {
		t.Fatalf("current window = H %d, %+v", now.H, now)
	}
	// The same rows by sequence: only those logged after h.
	var fresh Events
	if err := d.View(ctx, func(s *Snapshot) error {
		var err error
		fresh, err = s.Events(ctx, from, testNow, h, s.H)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(fresh.AI) != 1 || len(fresh.Activity) != 1 || len(fresh.Git) != 1 || len(fresh.Heartbeats) != 1 ||
		fresh.AI[0].Seq <= h || fresh.AI[0].Event.PromptHash != "ab" {
		t.Fatalf("rows after %d = %+v", h, fresh)
	}
}

// A write that hasn't committed is entirely outside a snapshot: neither its
// sequence number nor its row is visible, and the next snapshot after the
// commit sees both.
func TestUncommittedWriteIsOutsideSnapshot(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	if err := d.RecordActivity(ctx, *activity(minute(-3)).Activity); err != nil {
		t.Fatal(err)
	}
	tx, err := d.sql.BeginTx(ctx, nil) // immediate: holds the write lock
	if err != nil {
		t.Fatal(err)
	}
	w, err := openLog(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, qActivityInsert)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()
	if err := w.insert(ctx, stmt, eventArgs(activity(minute(-2)))...); err != nil {
		t.Fatal(err)
	}
	if err := w.close(ctx, tx, testNow); err != nil {
		t.Fatal(err)
	}
	read := func() Window {
		t.Helper()
		win, err := d.ReadWindow(ctx, minute(-60), testNow, 0)
		if err != nil {
			t.Fatal(err)
		}
		return win
	}
	if win := read(); win.H != 1 || len(win.Events.Activity) != 1 {
		t.Fatalf("snapshot during the write: H %d, %d samples; want 1, 1", win.H, len(win.Events.Activity))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if win := read(); win.H != 2 || len(win.Events.Activity) != 2 || win.Events.Activity[1].Seq != 2 {
		t.Fatalf("snapshot after the commit: %+v", win)
	}
}

// Late counts rows logged in a sequence range whose minute is final.
func TestLateRows(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	if _, err := d.SaveRun(ctx, Run{Key: Key{T: minute(0), H: 0}, From: minute(-60), To: minute(0),
		Removal: removal, FinalBefore: minute(-28)}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertEvents(ctx, []wire.LocalEvent{activity(minute(-40)), activity(minute(-20)), activity(minute(-29).Add(30 * time.Second))}); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(ctx, []wire.Heartbeat{{Entity: "/x", Time: float64(minute(-45).Unix()), Project: "app"}}, false); err != nil {
		t.Fatal(err)
	}
	var res LateResult
	if err := d.View(ctx, func(s *Snapshot) error {
		var err error
		res, err = s.Late(ctx, 0, s.H)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res.Count != 3 || !res.Latest.Equal(minute(-29).Add(30*time.Second)) {
		t.Fatalf("Late = %+v, want 3 (latest at -29:30)", res)
	}
	if err := d.View(ctx, func(s *Snapshot) error {
		var err error
		res, err = s.Late(ctx, 2, 3)
		return err
	}); err != nil || res.Count != 1 {
		t.Fatalf("Late(2, 3] = %+v, %v; want 1", res, err)
	}
}

// Retention never breaks the log: the row cap and pruning delete only rows
// the device's chain has sealed. A full table of unsealed rows drops new
// activity samples, counted, and keeps every other kind of row.
func TestRetentionDeletesOnlySealedRows(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t, WithEventCap(3))
	for i := range 5 {
		// Over CapHorizon old, so the cap may trim them once sealed.
		if err := d.RecordActivity(ctx, *activity(minute(-300 + i)).Activity); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, d.sql, "activity_samples"); n != 3 {
		t.Fatalf("activity samples = %d, want 3 (cap)", n)
	}
	if n, _ := d.Counter(ctx, KeyDroppedSamples); n != 2 {
		t.Fatalf("dropped samples = %d, want 2", n)
	}
	if h := high(ctx, t, d); h != 3 {
		t.Fatalf("H = %d: a dropped sample took a sequence number", h)
	}
	for i := range 5 {
		ai := wire.LocalEvent{Type: "ai", AI: &wire.AIEvent{Time: minute(-i), Harness: wire.HarnessCodex, Kind: wire.AIToolStart}}
		if err := d.InsertEvents(ctx, []wire.LocalEvent{ai}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, d.sql, "ai_events"); n != 5 {
		t.Fatalf("ai events = %d, want all 5 kept", n)
	}
	if g, _ := d.EventGeneration(ctx); g != 0 {
		t.Fatalf("generation = %d with nothing deleted", g)
	}

	// Once sealed, the oldest rows over the cap go, and new samples fit.
	seal(t, d, high(ctx, t, d))
	if err := d.RecordActivity(ctx, *activity(minute(-1)).Activity); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d.sql, "activity_samples"); n != 3 {
		t.Fatalf("activity samples after sealing = %d, want 3", n)
	}
	if n, _ := d.Counter(ctx, KeyDroppedSamples); n != 2 {
		t.Fatalf("dropped samples = %d, want still 2", n)
	}
	if g, _ := d.EventGeneration(ctx); g != 1 {
		t.Fatalf("generation after a trim = %d, want 1", g)
	}
	// The trim raised the deletion floor to its horizon, so no computation
	// reads below what was deleted.
	if f := floor(ctx, t, d); !f.Equal(testNow.Add(-CapHorizon)) {
		t.Fatalf("floor = %v, want %v", f, testNow.Add(-CapHorizon))
	}
}

// The row cap never deletes rows younger than CapHorizon, even when they
// are sealed: open minutes still read them, and a replay at their key reads
// them from the sealed blocks.
func TestCapKeepsRecentSealedRows(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t, WithEventCap(2))
	for i := range 3 {
		if err := d.RecordActivity(ctx, *activity(minute(-30 + i)).Activity); err != nil {
			t.Fatal(err)
		}
	}
	seal(t, d, high(ctx, t, d))
	if err := d.RecordActivity(ctx, *activity(minute(-1)).Activity); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d.sql, "activity_samples"); n != 2 {
		t.Fatalf("activity samples = %d, want 2", n)
	}
	w, err := d.ReadWindow(ctx, minute(-120), minute(0), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Events.Activity) != 2 {
		t.Fatalf("window holds %d samples, want the two kept", len(w.Events.Activity))
	}
	if f := floor(ctx, t, d); !f.Equal(time.UnixMilli(0).UTC()) {
		t.Fatalf("floor = %v with nothing deleted", f)
	}
}

func floor(ctx context.Context, t *testing.T, d *DB) time.Time {
	t.Helper()
	var f time.Time
	if err := d.View(ctx, func(s *Snapshot) error { f = s.Floor; return nil }); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPruneOnlySealedRows(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	old := testNow.Add(-100 * 24 * time.Hour)
	if err := d.InsertEvents(ctx, []wire.LocalEvent{activity(old), activity(minute(-1))}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertActiveTime(ctx, []ActiveTimePoint{{From: old, To: old.Add(time.Minute), SessionID: "s", Type: ActiveCLI, Secs: 1}}); err != nil {
		t.Fatal(err)
	}
	stale := hb(1)
	stale.Time = float64(old.Unix())
	if err := d.Enqueue(ctx, []wire.Heartbeat{stale}, false); err != nil {
		t.Fatal(err)
	}
	if r, err := d.Prune(ctx); err != nil || r.ActivitySamples != 0 || r.ActiveTime != 0 {
		t.Fatalf("Prune before sealing = %+v, %v", r, err)
	}
	if r, err := d.PruneCrediting(ctx); err != nil || r.Heartbeats != 0 {
		t.Fatalf("PruneCrediting before sealing = %+v, %v", r, err)
	}
	seal(t, d, high(ctx, t, d))
	if r, err := d.Prune(ctx); err != nil || r.ActivitySamples != 1 || r.ActiveTime != 1 {
		t.Fatalf("Prune after sealing = %+v, %v", r, err)
	}
	if r, err := d.PruneCrediting(ctx); err != nil || r.Heartbeats != 1 {
		t.Fatalf("PruneCrediting after sealing = %+v, %v", r, err)
	}
	if g, _ := d.EventGeneration(ctx); g != 2 {
		t.Fatalf("generation = %d, want 2", g)
	}
}

func TestRepoMappingAtHighWater(t *testing.T) {
	ctx := context.Background()
	clock := testNow
	d, _ := openTest(t, WithClock(func() time.Time { return clock }))
	t1 := testNow.Add(-2 * time.Hour)
	record := func(at time.Time, repos ...struggle.Repo) int64 {
		t.Helper()
		if err := d.RecordRepos(ctx, at, repos); err != nil {
			t.Fatal(err)
		}
		return high(ctx, t, d)
	}
	reposAt := func(h int64) []struggle.Repo {
		t.Helper()
		var out []struggle.Repo
		if err := d.View(ctx, func(s *Snapshot) error {
			var err error
			out, err = s.Repos(ctx, h)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	h1 := record(t1, struggle.Repo{Path: "/src/app", Project: "old"}, struggle.Repo{Path: "", Project: "x"})
	if h := record(t1.Add(time.Minute), struggle.Repo{Path: "/src/app", Project: "old"}); h != h1 {
		t.Fatal("an unchanged mapping was logged again")
	}
	h2 := record(t1.Add(time.Hour), struggle.Repo{Path: "/src/app", Project: "new"}, struggle.Repo{Path: "/src/lib", Project: "lib"})
	if got := reposAt(h1); !slices.Equal(got, []struggle.Repo{{Path: "/src/app", Project: "old"}}) {
		t.Fatalf("repos at %d = %+v", h1, got)
	}
	if got := reposAt(h2); !slices.Equal(got, []struggle.Repo{{Path: "/src/app", Project: "new"}, {Path: "/src/lib", Project: "lib"}}) {
		t.Fatalf("repos at %d = %+v", h2, got)
	}
	// "old" applies until the "new" row; it goes only once sealed and that
	// row is older than the retention cut.
	clock = t1.Add(time.Hour).Add(DefaultRetention).Add(time.Minute)
	if _, err := d.PruneCrediting(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d.sql, "repo_map"); n != 3 {
		t.Fatalf("pruned an unsealed row: %d rows", n)
	}
	seal(t, d, h2)
	clock = t1.Add(time.Hour).Add(DefaultRetention)
	if _, err := d.PruneCrediting(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d.sql, "repo_map"); n != 3 {
		t.Fatalf("pruned too early: %d rows", n)
	}
	clock = clock.Add(time.Minute)
	if _, err := d.PruneCrediting(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reposAt(h2); countRows(t, d.sql, "repo_map") != 2 || got[0].Project != "new" {
		t.Fatalf("after prune = %+v", got)
	}
}
