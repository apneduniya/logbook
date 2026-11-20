package localdb

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

func TestHeartbeatsKeptInFullAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	a, b := hb(1), hb(2)
	a.IsWrite, a.Lineno = true, new(7)
	if err := d.Enqueue(ctx, []wire.Heartbeat{a, b}, false); err != nil {
		t.Fatal(err)
	}
	// wakatime-cli resends a heartbeat after a failed request.
	if err := d.Enqueue(ctx, []wire.Heartbeat{a}, false); err != nil {
		t.Fatal(err)
	}
	ev, err := d.EventsBetween(ctx, testNow.Add(-time.Hour), testNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Heartbeats) != 2 {
		t.Fatalf("heartbeats = %d, want 2 (deduplicated)", len(ev.Heartbeats))
	}
	if got := ev.Heartbeats[0]; got.Entity != a.Entity || !got.IsWrite || got.Lineno == nil || *got.Lineno != 7 || got.Time != a.Time {
		t.Fatalf("heartbeat not kept in full: %+v", got)
	}
}

func TestEventsBetweenHighWater(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	at := testNow.Add(-30 * time.Minute)
	evs := []wire.LocalEvent{
		{Type: "ai", AI: &wire.AIEvent{Time: at, Harness: wire.HarnessCodex, Kind: wire.AIPromptSubmit, SessionID: "s",
			Project: "app", Cwd: "/src/app", Model: "m", Tool: "t", PromptChars: 5, Decision: "allow"}},
		{Type: "activity", Activity: &wire.ActivitySample{Time: at, IdleSeconds: 2.5, FocusApp: "Code", FocusKind: "editor"}},
		{Type: "git", Git: &wire.GitEvent{Time: at, Project: "app", Kind: "worktree", FilesChanged: 1, Insertions: 2, Deletions: 3}},
		{Type: "activity", Activity: &wire.ActivitySample{Time: testNow, FocusKind: "other"}}, // at `to`: excluded
	}
	if err := d.InsertEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(ctx, []wire.Heartbeat{hb(1)}, false); err != nil {
		t.Fatal(err)
	}
	hw, err := d.HighWater(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Late events after the high-water mark.
	if err := d.InsertEvents(ctx, evs[:3]); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(ctx, []wire.Heartbeat{hb(3)}, false); err != nil {
		t.Fatal(err)
	}
	from := testNow.Add(-time.Hour)
	pinned, err := d.EventsBetween(ctx, from, testNow, &hw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned.AI) != 1 || len(pinned.Activity) != 1 || len(pinned.Git) != 1 || len(pinned.Heartbeats) != 1 {
		t.Fatalf("pinned events = %d ai, %d activity, %d git, %d heartbeats", len(pinned.AI), len(pinned.Activity),
			len(pinned.Git), len(pinned.Heartbeats))
	}
	if got := pinned.AI[0]; got != *evs[0].AI {
		t.Fatalf("ai event round trip = %+v, want %+v", got, *evs[0].AI)
	}
	if got := pinned.Activity[0]; got != *evs[1].Activity {
		t.Fatalf("activity round trip = %+v", got)
	}
	if got := pinned.Git[0]; got != *evs[2].Git {
		t.Fatalf("git round trip = %+v", got)
	}
	all, err := d.EventsBetween(ctx, from, testNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.AI) != 2 || len(all.Activity) != 2 || len(all.Heartbeats) != 2 {
		t.Fatalf("unpinned events = %d ai, %d activity, %d heartbeats", len(all.AI), len(all.Activity), len(all.Heartbeats))
	}
}

func minute(i int) time.Time { return testNow.Add(time.Duration(i) * time.Minute) }

func cm(i int, project string, credited int) CreditedMinute {
	return CreditedMinute{Minute: minute(i), Project: project, Label: "you", Credited: credited, Cuts: []string{},
		Flags: []string{"low_signal"}, RulesVersion: "v1", Digest: fmt.Sprintf("%s-%d", project, credited)}
}

func superseded(old CreditedMinute) CreditedMinute {
	return CreditedMinute{Minute: old.Minute, Project: old.Project, Label: "idle", RulesVersion: "v1", Digest: "zero"}
}

func TestSaveMinutesOpenAndFinal(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	upd := func(ms ...CreditedMinute) SaveResult {
		t.Helper()
		r, err := d.SaveMinutes(ctx, MinuteUpdate{From: minute(-60), To: minute(0), OpenFrom: minute(-30),
			Minutes: ms, Superseded: superseded})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// First compute: one final minute, two open ones.
	if r := upd(cm(-40, "a", 60), cm(-10, "a", 60), cm(-5, "b", 30)); r.Inserted != 3 {
		t.Fatalf("first save = %+v", r)
	}
	unsent, err := d.UnsentMinutes(ctx, minute(-30), 10)
	if err != nil || len(unsent) != 3 {
		t.Fatalf("unsent = %v, %v", unsent, err)
	}
	if unsent[0].Flags[0] != "low_signal" || len(unsent[0].Cuts) != 0 {
		t.Fatalf("codes round trip = %+v", unsent[0])
	}
	// The server accepts minute -10 only.
	if err := d.MarkMinutesSent(ctx, unsent[1:2], minute(-30)); err != nil {
		t.Fatal(err)
	}
	// Recompute: the final minute changed (ignored), -10 moved from a to c,
	// -5 changed credit.
	r := upd(cm(-40, "a", 10), cm(-10, "c", 60), cm(-5, "b", 40))
	if r.Inserted != 1 || r.Updated != 2 || r.Deleted != 0 {
		t.Fatalf("recompute = %+v", r)
	}
	unsent, _ = d.UnsentMinutes(ctx, minute(-30), 10)
	want := []struct {
		m        int
		project  string
		credited int
		label    string
	}{{-40, "a", 60, "you"}, {-10, "a", 0, "idle"}, {-10, "c", 60, "you"}, {-5, "b", 40, "you"}}
	if len(unsent) != len(want) {
		t.Fatalf("unsent = %+v", unsent)
	}
	for i, w := range want {
		u := unsent[i]
		if !u.Minute.Equal(minute(w.m)) || u.Project != w.project || u.Credited != w.credited || u.Label != w.label {
			t.Fatalf("unsent[%d] = %+v, want %+v", i, u, w)
		}
	}
	// A minute that was never sent and disappears is deleted.
	if r := upd(cm(-40, "a", 60), cm(-10, "c", 60)); r.Deleted != 1 {
		t.Fatalf("drop unsent = %+v", r)
	}
}

func TestUnsentMinutesSettlesFinal(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	if _, err := d.SaveMinutes(ctx, MinuteUpdate{From: minute(-60), To: minute(0), OpenFrom: minute(-30),
		Minutes: []CreditedMinute{cm(-20, "a", 60)}, Superseded: superseded}); err != nil {
		t.Fatal(err)
	}
	ms, _ := d.UnsentMinutes(ctx, minute(-30), 10)
	if err := d.MarkMinutesSent(ctx, ms, minute(-30)); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.UnsentMinuteCount(ctx); n != 0 {
		t.Fatalf("unsent count = %d", n)
	}
	// Twenty minutes later the minute is final and unchanged: settled, not resent.
	ms, err := d.UnsentMinutes(ctx, minute(-10), 10)
	if err != nil || len(ms) != 0 {
		t.Fatalf("UnsentMinutes = %v, %v", ms, err)
	}
	var final int
	if err := d.sql.QueryRowContext(ctx, `SELECT sent_final FROM credited_minutes`).Scan(&final); err != nil || final != 1 {
		t.Fatalf("sent_final = %d, %v", final, err)
	}
	if err := d.RetireMinutesBefore(ctx, minute(0)); err != nil {
		t.Fatal(err)
	}
}

func TestUnsentMinutesWholeMinutes(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	if _, err := d.SaveMinutes(ctx, MinuteUpdate{From: minute(-60), To: minute(0), OpenFrom: minute(-30),
		Minutes: []CreditedMinute{
			cm(-10, "a", 30), cm(-10, "b", 30),
			cm(-9, "a", 60),
			cm(-8, "x", 10), cm(-8, "y", 20), cm(-8, "z", 30),
		}, Superseded: superseded}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		limit int
		want  []string // "minute/project"
	}{
		{1, []string{"-10/a", "-10/b"}}, // a first minute longer than the limit comes whole
		{2, []string{"-10/a", "-10/b"}},
		{3, []string{"-10/a", "-10/b", "-9/a"}},
		{4, []string{"-10/a", "-10/b", "-9/a"}}, // -8 would be split: left for the next batch
		{5, []string{"-10/a", "-10/b", "-9/a"}},
		{6, []string{"-10/a", "-10/b", "-9/a", "-8/x", "-8/y", "-8/z"}},
		{100, []string{"-10/a", "-10/b", "-9/a", "-8/x", "-8/y", "-8/z"}},
	}
	for _, tt := range tests {
		ms, err := d.UnsentMinutes(ctx, minute(-30), tt.limit)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(ms))
		for i, m := range ms {
			got[i] = fmt.Sprintf("%d/%s", int(m.Minute.Sub(testNow)/time.Minute), m.Project)
		}
		if fmt.Sprint(got) != fmt.Sprint(tt.want) {
			t.Errorf("UnsentMinutes(limit %d) = %v, want %v", tt.limit, got, tt.want)
		}
	}
}

// TestAIEventPromptHashRoundTrip guards the merge with the signals module,
// whose migration 0003 adds ai_events.prompt_hash: once the column exists,
// both event reads must return it, or the engine's repeated-prompt cut can
// never fire and evidence hashes leave the field out.
func TestAIEventPromptHashRoundTrip(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	var hasColumn int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('ai_events') WHERE name = 'prompt_hash'`).Scan(&hasColumn); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	at := testNow.Add(-time.Minute)
	if err := d.InsertEvents(ctx, []wire.LocalEvent{{Type: "ai", AI: &wire.AIEvent{Time: at,
		Harness: wire.HarnessCodex, Kind: wire.AIPromptSubmit, SessionID: "s", PromptHash: hash}}}); err != nil {
		t.Fatal(err)
	}
	want := ""
	if hasColumn == 1 {
		want = hash
	}
	hw, err := d.HighWater(ctx)
	if err != nil {
		t.Fatal(err)
	}
	between, err := d.EventsBetween(ctx, at, testNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := d.EventsAfter(ctx, HighWater{}, hw, at, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for name, ev := range map[string]Events{"EventsBetween": between, "EventsAfter": after} {
		if len(ev.AI) != 1 || ev.AI[0].PromptHash != want {
			t.Errorf("%s: AI events = %+v, want prompt hash %q (column present: %v)", name, ev.AI, want, hasColumn == 1)
		}
	}
}

func TestEvidenceDays(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	const dev = "0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b"
	if _, ok, err := d.LastEvidenceDay(ctx, dev); ok || err != nil {
		t.Fatalf("LastEvidenceDay(empty) = %v, %v", ok, err)
	}
	e := EvidenceDay{DeviceID: dev, Day: "2026-11-18", ContentHash: "c", ChainHash: "h", Signature: []byte{1, 2},
		RulesVersion: "v1", EventCounts: map[string]int{"ai": 3}, HighWater: HighWater{1, 2, 3, 4}, State: EvidencePending}
	if err := d.InsertEvidenceDay(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertEvidenceDay(ctx, e); err == nil {
		t.Fatal("second insert for the same day succeeded")
	}
	e2 := e
	e2.Day, e2.PrevChainHash = "2026-11-19", "h"
	if err := d.InsertEvidenceDay(ctx, e2); err != nil {
		t.Fatal(err)
	}
	if err := d.SetEvidenceState(ctx, dev, "2026-11-19", EvidenceAccepted, "", KV{"evidence.last_day", "2026-11-19"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := d.LastEvidenceDay(ctx, dev)
	if err != nil || !ok || got.Day != "2026-11-19" || got.State != EvidenceAccepted || got.HighWater != e.HighWater ||
		got.EventCounts["ai"] != 3 || string(got.Signature) != "\x01\x02" {
		t.Fatalf("LastEvidenceDay = %+v, %v, %v", got, ok, err)
	}
	if v, _ := d.Get(ctx, "evidence.last_day"); v != "2026-11-19" {
		t.Fatalf("mark = %q", v)
	}
	if err := d.SetEvidenceState(ctx, dev, "2026-01-01", EvidenceAccepted, ""); err == nil {
		t.Fatal("updating a missing day succeeded")
	}
}

func TestPruneCrediting(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	old := hb(1)
	old.Time = float64(testNow.Add(-100 * 24 * time.Hour).Unix())
	if err := d.Enqueue(ctx, []wire.Heartbeat{old, hb(2)}, false); err != nil {
		t.Fatal(err)
	}
	oldMinute := testNow.Add(-100 * 24 * time.Hour).Truncate(time.Minute)
	if _, err := d.SaveMinutes(ctx, MinuteUpdate{From: oldMinute, To: oldMinute.Add(time.Minute), OpenFrom: oldMinute,
		Minutes:    []CreditedMinute{{Minute: oldMinute, Project: "a", Label: "you", RulesVersion: "v1", Digest: "x"}},
		Superseded: superseded}); err != nil {
		t.Fatal(err)
	}
	r, err := d.PruneCrediting(ctx)
	if err != nil || r != (CreditPruneResult{Heartbeats: 1, CreditedMinutes: 1}) {
		t.Fatalf("PruneCrediting = %+v, %v", r, err)
	}
}
