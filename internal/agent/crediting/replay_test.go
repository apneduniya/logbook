package crediting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/wire"
)

// A sealed day must rebuild to the same bytes later, whatever arrives after
// sealing: late active-time points (OTLP export interval, delta
// temporality), late heartbeats (wakatime-cli's offline queue) and late
// hook events, which also change derived values such as a session's
// working directory.
func TestSealedDayRebuildsIdentically(t *testing.T) {
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

	e.mu.Lock()
	e.now = time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC)
	e.mu.Unlock()
	e.run()
	id := e.deviceID()
	days := e.srv.storedDays(id)
	if len(days) != 1 || days[0].EventCounts[evidence.TypeActiveTime] != 1 {
		t.Fatalf("server days = %+v, want the 19th with one active-time point", days)
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
	events, err := ReplayDay(ctx, e.db, sealed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := evidence.ContentHash(events)
	if err != nil {
		t.Fatal(err)
	}
	if got != sealed.ContentHash || got != days[0].ContentHash {
		t.Fatalf("rebuilt content hash = %s, sealed %s", got, sealed.ContentHash)
	}
	if n := evidence.Counts(events)[evidence.TypeActiveTime]; n != 1 || events.ActiveTime[0].ProjectCwd != "/src/app" {
		t.Fatalf("rebuilt active time = %+v", events.ActiveTime)
	}

	// A day whose data is no longer all there says so instead of hashing
	// something else.
	legacy := sealed
	legacy.ActiveTime = nil
	if _, err := ReplayDay(ctx, e.db, legacy); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("replay without kept active time: %v", err)
	}
	short := sealed
	short.HighWater.Heartbeats = 0
	if _, err := ReplayDay(ctx, e.db, short); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("replay with missing rows: %v", err)
	}

	// The day keeps its minutes' cutoffs (none differ from the default
	// here: the scripted engine credits nothing on the 19th), so its credit
	// can be replayed; a day sealed before cutoffs were kept can't.
	if sealed.Cutoffs == nil {
		t.Fatal("sealed day kept no cutoffs")
	}
	if _, err := ReplayDayCredit(ctx, e.db, sealed, nil, nil, time.UTC); err != nil {
		t.Fatalf("ReplayDayCredit: %v", err)
	}
	noCutoffs := sealed
	noCutoffs.Cutoffs = nil
	if _, err := ReplayDayCredit(ctx, e.db, noCutoffs, nil, nil, time.UTC); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("credit replay without kept cutoffs: %v", err)
	}
}
