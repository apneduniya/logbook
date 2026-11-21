package localdb

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

func at(m int) time.Time { return testNow.Add(time.Duration(m) * time.Minute) }

func TestActiveTimesRangeOrderAndCwd(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	// Hook events give session s1 a working directory; the latest one before
	// the window's end applies to all its points.
	evs := []wire.LocalEvent{
		{Type: "ai", AI: &wire.AIEvent{Time: at(-50), Harness: wire.HarnessClaudeCode, Kind: wire.AISessionStart, SessionID: "s1", Cwd: "/w/app"}},
		{Type: "ai", AI: &wire.AIEvent{Time: at(-15), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit, SessionID: "s1", Cwd: "/w/app/sub"}},
		// Same session id from another harness must not be used.
		{Type: "ai", AI: &wire.AIEvent{Time: at(-40), Harness: wire.HarnessCodex, Kind: wire.AISessionStart, SessionID: "s2", Cwd: "/w/other"}},
	}
	if err := d.InsertEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkReceiverUp(ctx, at(-120), at(0)); err != nil {
		t.Fatal(err)
	}
	pts := []ActiveTimePoint{
		{From: at(-30), To: at(-29), SessionID: "s1", Type: ActiveCLI, Secs: 40},
		{From: at(-30), To: at(-29), SessionID: "s1", Type: ActiveUser, Secs: 12.5},
		{From: at(-10), To: at(-9), SessionID: "s1", Type: ActiveUser, Secs: 30},
		{From: at(-30), To: at(-29), SessionID: "s2", Type: ActiveUser, Secs: 5},
		{From: at(-90), To: at(-80), SessionID: "s1", Type: ActiveUser, Secs: 99}, // before the window
		{From: at(-5), To: at(-5), SessionID: "s3", Type: ActiveUser, Secs: 1},    // zero-length, inside
		{From: at(0), To: at(1), SessionID: "s1", Type: ActiveUser, Secs: 7},      // at the window's end
	}
	if err := d.InsertActiveTime(ctx, pts); err != nil {
		t.Fatal(err)
	}
	// A retried export is stored once.
	if err := d.InsertActiveTime(ctx, pts[:1]); err != nil {
		t.Fatal(err)
	}
	got, err := d.ActiveTimes(ctx, at(-60), at(0))
	if err != nil {
		t.Fatal(err)
	}
	cc := wire.HarnessClaudeCode
	want := []struggle.ActiveTime{
		{From: at(-30), To: at(-29), SessionID: "s1", CLISecs: 40, Harness: cc, ProjectCwd: "/w/app/sub"},
		{From: at(-30), To: at(-29), SessionID: "s1", UserSecs: 12.5, Harness: cc, ProjectCwd: "/w/app/sub"},
		{From: at(-30), To: at(-29), SessionID: "s2", UserSecs: 5, Harness: cc},
		{From: at(-10), To: at(-9), SessionID: "s1", UserSecs: 30, Harness: cc, ProjectCwd: "/w/app/sub"},
		{From: at(-5), To: at(-5), SessionID: "s3", UserSecs: 1, Harness: cc},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d points: %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("point %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLastCumulative(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	start := at(-60)
	if _, _, ok, err := d.LastCumulative(ctx, "s", ActiveUser, start); ok || err != nil {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	c1, c2 := 30.0, 75.0
	pts := []ActiveTimePoint{
		{From: start, To: at(-2), SessionID: "s", Type: ActiveUser, Secs: 30, SeriesStart: start, Cumulative: &c1},
		{From: at(-2), To: at(-1), SessionID: "s", Type: ActiveUser, Secs: 45, SeriesStart: start, Cumulative: &c2},
		{From: at(-1), To: at(0), SessionID: "s", Type: ActiveUser, Secs: 3}, // a delta point: not a series
	}
	if err := d.InsertActiveTime(ctx, pts); err != nil {
		t.Fatal(err)
	}
	to, total, ok, err := d.LastCumulative(ctx, "s", ActiveUser, start)
	if err != nil || !ok || !to.Equal(at(-1)) || total != 75 {
		t.Fatalf("LastCumulative = %v %v %v %v", to, total, ok, err)
	}
	if _, _, ok, _ := d.LastCumulative(ctx, "s", ActiveCLI, start); ok {
		t.Fatal("other type found")
	}
	if _, _, ok, _ := d.LastCumulative(ctx, "s", ActiveUser, at(-30)); ok {
		t.Fatal("other series start found")
	}
}

func TestInsertActiveTimeRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	good := ActiveTimePoint{From: at(-2), To: at(-1), SessionID: "s", Type: ActiveUser, Secs: 1}
	tests := map[string]func(*ActiveTimePoint){
		"no session":   func(p *ActiveTimePoint) { p.SessionID = "" },
		"bad type":     func(p *ActiveTimePoint) { p.Type = "idle" },
		"negative":     func(p *ActiveTimePoint) { p.Secs = -1 },
		"nan":          func(p *ActiveTimePoint) { p.Secs = math.NaN() },
		"reversed":     func(p *ActiveTimePoint) { p.To = p.From.Add(-time.Second) },
		"zero from":    func(p *ActiveTimePoint) { p.From = time.Time{} },
		"long session": func(p *ActiveTimePoint) { p.SessionID = string(make([]byte, 257)) },
	}
	for name, mutate := range tests {
		p := good
		mutate(&p)
		if err := d.InsertActiveTime(ctx, []ActiveTimePoint{good, p}); !errors.Is(err, ErrInvalidActiveTime) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Nothing from the failed batches was stored.
	if got, _ := d.ActiveTimes(ctx, at(-60), at(60)); len(got) != 0 {
		t.Fatalf("stored %d points", len(got))
	}
}

func TestActiveTimePruneAndCap(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t, WithEventCap(3))
	seal(t, d, 1000) // everything this test stores counts as sealed
	var pts []ActiveTimePoint
	for i := range 5 {
		// Over CapHorizon old, so the cap may trim them.
		pts = append(pts, ActiveTimePoint{From: at(-300 + i), To: at(-299 + i), SessionID: "s", Type: ActiveUser, Secs: 1})
	}
	pts = append(pts, ActiveTimePoint{From: testNow.Add(-100 * 24 * time.Hour), To: testNow.Add(-100*24*time.Hour + time.Minute),
		SessionID: "old", Type: ActiveCLI, Secs: 1})
	for _, p := range pts {
		if err := d.InsertActiveTime(ctx, []ActiveTimePoint{p}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, d.sql, "active_time"); n != 3 {
		t.Fatalf("rows after cap = %d, want 3", n)
	}
	r, err := d.Prune(ctx)
	if err != nil || r.ActiveTime != 1 {
		t.Fatalf("Prune = %+v, %v", r, err)
	}
	if n := countRows(t, d.sql, "active_time"); n != 2 {
		t.Fatalf("rows after prune = %d, want 2", n)
	}
}

func TestPromptHashStored(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	h := "c496d67af0c2c9a5660e898528d134631f32a679c5738c03d8f5afcedbc66b79"
	ev := wire.AIEvent{Time: at(-1), Harness: wire.HarnessClaudeCode, Kind: wire.AIPromptSubmit, SessionID: "s", PromptChars: 3, PromptHash: h}
	if err := d.InsertEvents(ctx, []wire.LocalEvent{{Type: "ai", AI: &ev}}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.sql.QueryRowContext(ctx, `SELECT prompt_hash FROM ai_events`).Scan(&got); err != nil || got != h {
		t.Fatalf("prompt_hash = %q, %v", got, err)
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil { //nolint:gosec // fixed table names in tests
		t.Fatal(err)
	}
	return n
}
