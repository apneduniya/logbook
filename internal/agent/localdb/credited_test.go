package localdb

import (
	"context"
	"fmt"
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
	w, err := d.ReadWindow(ctx, testNow.Add(-time.Hour), testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Events.Heartbeats) != 2 || w.H != 2 {
		t.Fatalf("heartbeats = %d at H %d, want 2 (deduplicated)", len(w.Events.Heartbeats), w.H)
	}
	if got := w.Events.Heartbeats[0].Event; got.Entity != a.Entity || !got.IsWrite || got.Lineno == nil || *got.Lineno != 7 || got.Time != a.Time {
		t.Fatalf("heartbeat not kept in full: %+v", got)
	}
}

func minute(i int) time.Time { return testNow.Add(time.Duration(i) * time.Minute) }

func cm(i int, project string, credited int) CreditedMinute {
	return CreditedMinute{Minute: minute(i), Project: project, Label: "you", Credited: credited, Cuts: []string{},
		Flags: []string{"low_signal"}, RulesVersion: "v1", Digest: fmt.Sprintf("%s-%d", project, credited)}
}

func removal(at time.Time, project string) CreditedMinute {
	return CreditedMinute{Minute: at, Project: project, Label: "idle", RulesVersion: "v1", Digest: "removal"}
}

// run stores one engine run at T = minute(t) and log mark h.
func run(t *testing.T, d *DB, at int, h int64, settled bool, ms ...CreditedMinute) RunResult {
	t.Helper()
	r := Run{Key: Key{T: minute(at), H: h}, From: minute(at - 60), To: minute(at), Minutes: ms, Removal: removal}
	if settled {
		r.FinalBefore = minute(at - 28)
	}
	res, err := d.SaveRun(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func stored(t *testing.T, d *DB, i int) []CreditedMinute {
	t.Helper()
	ms, err := d.StoredMinutes(context.Background(), minute(i), minute(i+1))
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// An unchanged result keeps the stored rows and their older key; a changed
// one replaces all of the minute's rows under the new key.
func TestSaveRunKeysMinutes(t *testing.T) {
	d, _ := openTest(t)
	if res := run(t, d, 0, 10, false, cm(-5, "a", 30), cm(-5, "b", 20), cm(-4, "a", 60)); res.Replaced != 2 {
		t.Fatalf("first run = %+v", res)
	}
	if res := run(t, d, 1, 12, false, cm(-5, "a", 30), cm(-5, "b", 20), cm(-4, "a", 45)); res.Replaced != 1 || res.Kept != 1 {
		t.Fatalf("second run = %+v", res)
	}
	for _, m := range stored(t, d, -5) {
		if m.Key != (Key{T: minute(0), H: 10}) || m.Final || m.Acknowledged {
			t.Fatalf("unchanged minute -5 = %+v, want the first key, open", m)
		}
	}
	if m := stored(t, d, -4); len(m) != 1 || m[0].Credited != 45 || m[0].Key != (Key{T: minute(1), H: 12}) {
		t.Fatalf("changed minute -4 = %+v", m)
	}
	// One project of a minute changing moves the whole minute to the new key.
	run(t, d, 2, 13, false, cm(-5, "a", 30), cm(-5, "b", 25), cm(-4, "a", 45))
	for _, m := range stored(t, d, -5) {
		if m.Key != (Key{T: minute(2), H: 13}) {
			t.Fatalf("minute -5 rows = %+v, want all at the new key", stored(t, d, -5))
		}
	}
}

// Final minutes are never recomputed, also with no rows, and a settled run
// finalizes open minutes outside its window with their own keys.
func TestFinalMinutesAreNeverRecomputed(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	run(t, d, -100, 1, false, cm(-130, "old", 60))
	run(t, d, 0, 5, false, cm(-40, "a", 60))
	if res := run(t, d, 1, 6, true, cm(-40, "a", 60)); res.Finalized != 2 {
		t.Fatalf("settled run = %+v, want 2 minutes finalized", res)
	}
	if m := stored(t, d, -130); len(m) != 1 || !m[0].Final || m[0].Key != (Key{T: minute(-100), H: 1}) {
		t.Fatalf("minute outside the window = %+v, want final with its own key", m)
	}
	for _, i := range []int{-130, -59, -40, -28} {
		if ok, err := d.IsFinal(ctx, minute(i)); err != nil || !ok {
			t.Fatalf("minute %d final = %v, %v", i, ok, err)
		}
	}
	for _, i := range []int{-131, -129, -60, -27} {
		if ok, _ := d.IsFinal(ctx, minute(i)); ok {
			t.Fatalf("minute %d final", i)
		}
	}
	// Later runs, also with the clock set back, change no final minute and
	// fill no empty one.
	for _, at := range []int{2, -20, 3} {
		if res := run(t, d, at, 9, true, cm(-40, "a", 10), cm(-41, "b", 60)); res.Replaced != 0 {
			t.Fatalf("run at %d rewrote a final minute: %+v", at, res)
		}
	}
	if m := stored(t, d, -40); len(m) != 1 || m[0].Credited != 60 || m[0].Key != (Key{T: minute(0), H: 5}) {
		t.Fatalf("final minute -40 = %+v", m)
	}
	if m := stored(t, d, -41); len(m) != 0 {
		t.Fatalf("empty final minute -41 filled: %+v", m)
	}
}

// A project the server holds that drops out of an open minute's result is
// replaced by a removal; once the server accepts the removal, the row goes.
func TestRemovalsAndAcknowledgement(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	run(t, d, 0, 1, false, cm(-5, "a", 30), cm(-5, "b", 30))
	rows, err := d.UnsentMinutes(ctx, time.Time{}, 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("UnsentMinutes = %+v, %v", rows, err)
	}
	if err := d.MarkMinutesSent(ctx, rows, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.UnsentMinuteCount(ctx); n != 0 {
		t.Fatalf("unsent after acceptance = %d", n)
	}
	// An unchanged result is not sent again.
	run(t, d, 1, 2, false, cm(-5, "a", 30), cm(-5, "b", 30))
	if n, _ := d.UnsentMinuteCount(ctx); n != 0 {
		t.Fatalf("unchanged minute queued again: %d", n)
	}
	// b drops out: a removal replaces it, with the minute's new key.
	run(t, d, 2, 3, false, cm(-5, "a", 60))
	rows, _ = d.UnsentMinutes(ctx, time.Time{}, 100)
	if len(rows) != 2 || rows[0].Project != "b" || rows[0].Label != "idle" || rows[0].Key.H != 3 || rows[1].Credited != 60 {
		t.Fatalf("rows after b dropped = %+v", rows)
	}
	// The same result again keeps the removal pending, under the same key.
	if res := run(t, d, 3, 4, false, cm(-5, "a", 60)); res.Kept != 1 {
		t.Fatalf("rerun = %+v", res)
	}
	if err := d.MarkMinutesSent(ctx, rows, nil); err != nil {
		t.Fatal(err)
	}
	if m := stored(t, d, -5); len(m) != 1 || m[0].Project != "a" || !m[0].Acknowledged {
		t.Fatalf("after the removal was accepted = %+v", m)
	}
	// A project the server never saw just goes.
	run(t, d, 4, 5, false, cm(-5, "a", 60), cm(-5, "c", 0))
	run(t, d, 5, 6, false, cm(-5, "a", 60))
	if m := stored(t, d, -5); len(m) != 1 {
		t.Fatalf("unsent project kept: %+v", m)
	}
	// A row rewritten since it was read is not marked sent.
	run(t, d, 6, 7, false, cm(-4, "a", 10))
	stale, _ := d.UnsentMinutes(ctx, time.Time{}, 100)
	run(t, d, 7, 8, false, cm(-4, "a", 20))
	if err := d.MarkMinutesSent(ctx, stale, nil); err != nil {
		t.Fatal(err)
	}
	if m := stored(t, d, -4); m[0].Acknowledged {
		t.Fatalf("stale answer acknowledged the new row: %+v", m)
	}
	// A refusal for good is answered, not accepted.
	cur, _ := d.UnsentMinutes(ctx, time.Time{}, 100)
	if err := d.MarkMinutesSent(ctx, nil, cur); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.UnsentMinuteCount(ctx); n != 0 {
		t.Fatalf("unsent after a refusal = %d", n)
	}
}

func TestAdoptMinute(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	run(t, d, 0, 4, false, cm(-5, "a", 30), cm(-5, "b", 30))
	server := cm(-5, "a", 45)
	server.Key = Key{T: minute(-3), H: 2}
	unkeyed := cm(-5, "z", 15)
	if err := d.AdoptMinute(ctx, minute(-5), []CreditedMinute{server, unkeyed}); err != nil {
		t.Fatal(err)
	}
	m := stored(t, d, -5)
	if len(m) != 2 || m[0].Credited != 45 || m[0].Key != server.Key || !m[0].Final || !m[0].Acknowledged ||
		m[1].Project != "z" || !m[1].Key.IsZero() {
		t.Fatalf("adopted minute = %+v", m)
	}
	if ok, _ := d.IsFinal(ctx, minute(-5)); !ok {
		t.Fatal("adopted minute not final")
	}
	if res := run(t, d, 1, 5, false, cm(-5, "a", 30)); res.Replaced != 0 {
		t.Fatalf("an adopted minute was recomputed: %+v", res)
	}
	if n, _ := d.UnsentMinuteCount(ctx); n != 0 {
		t.Fatalf("adopted rows queued: %d", n)
	}
}

// Open minutes before the hold are not sent while the agent settles; final
// ones always are.
func TestUnsentMinutesHoldAndWholeMinutes(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	run(t, d, 0, 1, false,
		cm(-10, "a", 30), cm(-10, "b", 30),
		cm(-9, "a", 60),
		cm(-8, "x", 10), cm(-8, "y", 20), cm(-8, "z", 30))
	tests := []struct {
		limit int
		want  []string // "minute/project"
	}{
		{1, []string{"-10/a", "-10/b"}}, // a first minute longer than the limit comes whole
		{2, []string{"-10/a", "-10/b"}},
		{3, []string{"-10/a", "-10/b", "-9/a"}},
		{4, []string{"-10/a", "-10/b", "-9/a"}}, // -8 would be split: left for the next batch
		{6, []string{"-10/a", "-10/b", "-9/a", "-8/x", "-8/y", "-8/z"}},
		{100, []string{"-10/a", "-10/b", "-9/a", "-8/x", "-8/y", "-8/z"}},
	}
	names := func(ms []CreditedMinute) string {
		got := make([]string, len(ms))
		for i, m := range ms {
			got[i] = fmt.Sprintf("%d/%s", int(m.Minute.Sub(testNow)/time.Minute), m.Project)
		}
		return fmt.Sprint(got)
	}
	for _, tt := range tests {
		ms, err := d.UnsentMinutes(ctx, time.Time{}, tt.limit)
		if err != nil {
			t.Fatal(err)
		}
		if names(ms) != fmt.Sprint(tt.want) {
			t.Errorf("UnsentMinutes(limit %d) = %v, want %v", tt.limit, names(ms), tt.want)
		}
	}
	held, _ := d.UnsentMinutes(ctx, minute(-9), 100)
	if names(held) != fmt.Sprint([]string{"-9/a", "-8/x", "-8/y", "-8/z"}) {
		t.Fatalf("held before -9: %s", names(held))
	}
	run(t, d, 60, 2, true) // makes -10 .. -8 final, outside its window
	all, _ := d.UnsentMinutes(ctx, minute(0), 100)
	if len(all) != 6 {
		t.Fatalf("final minutes held: %s", names(all))
	}
}

func TestMinutesPendingAndRetire(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)
	run(t, d, 0, 1, false, cm(-10, "a", 30), cm(-5, "a", 30))
	if n, _ := d.MinutesPendingBefore(ctx, minute(-9)); n != 1 {
		t.Fatalf("pending before -9 = %d, want 1", n)
	}
	if err := d.RetireMinutesBefore(ctx, minute(0)); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.MinutesPendingBefore(ctx, minute(-9)); n != 1 {
		t.Fatalf("an acknowledged open minute counts as settled: %d", n)
	}
	run(t, d, 30, 2, true, cm(-10, "a", 30), cm(-5, "a", 30))
	if n, _ := d.MinutesPendingBefore(ctx, minute(0)); n != 0 {
		t.Fatalf("pending after finalizing = %d", n)
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
		RulesVersion: "v1", EventCounts: map[string]int{"ai": 3}, SeqFrom: 1, SeqTo: 40, State: EvidencePending}
	if err := d.InsertEvidenceDay(ctx, e); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.Counter(ctx, KeySealed); n != 40 {
		t.Fatalf("sealed = %d, want 40", n)
	}
	if err := d.InsertEvidenceDay(ctx, e); err == nil {
		t.Fatal("second insert for the same day succeeded")
	}
	e2 := e
	e2.Day, e2.PrevChainHash, e2.SeqFrom, e2.SeqTo = "2026-11-19", "h", 41, 52
	if err := d.InsertEvidenceDay(ctx, e2); err != nil {
		t.Fatal(err)
	}
	if err := d.SetEvidenceState(ctx, dev, "2026-11-19", EvidenceAccepted, "", KV{"evidence.last_day", "2026-11-19"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := d.LastEvidenceDay(ctx, dev)
	if err != nil || !ok || got.Day != "2026-11-19" || got.State != EvidenceAccepted || got.SeqFrom != 41 || got.SeqTo != 52 ||
		got.Legacy || got.EventCounts["ai"] != 3 || string(got.Signature) != "\x01\x02" {
		t.Fatalf("LastEvidenceDay = %+v, %v, %v", got, ok, err)
	}
	if v, _ := d.Get(ctx, "evidence.last_day"); v != "2026-11-19" {
		t.Fatalf("mark = %q", v)
	}
	if got, ok, _ := d.EvidenceDayOf(ctx, dev, "2026-11-18"); !ok || got.SeqTo != 40 {
		t.Fatalf("EvidenceDayOf = %+v, %v", got, ok)
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
	seal(t, d, 2)
	oldMinute := testNow.Add(-100 * 24 * time.Hour).Truncate(time.Minute)
	if _, err := d.SaveRun(ctx, Run{Key: Key{T: oldMinute.Add(time.Minute), H: 2}, From: oldMinute, To: oldMinute.Add(time.Minute),
		Minutes:     []CreditedMinute{{Minute: oldMinute, Project: "a", Label: "you", RulesVersion: "v1", Digest: "x"}},
		Removal:     removal,
		FinalBefore: oldMinute.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	r, err := d.PruneCrediting(ctx)
	if err != nil || r != (CreditPruneResult{Heartbeats: 1, CreditedMinutes: 1}) {
		t.Fatalf("PruneCrediting = %+v, %v", r, err)
	}
	if ok, _ := d.IsFinal(ctx, oldMinute); ok {
		t.Fatal("final span of a pruned minute kept")
	}
}
