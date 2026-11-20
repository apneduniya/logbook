package crediting

import (
	"net/http"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/struggle"
)

// A day's evidence seals it on the server against first-time minutes, so
// it may go up only once every minute of the day has been accepted. With a
// backlog bigger than one run's upload budget, the day waits for the next
// run instead of stranding the rest of its minutes.
func TestDaySealedOnlyAfterItsMinutesAreAccepted(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 19, 23, 30, 5, 0, time.UTC))
	e.options = func(o *Options) { o.Batch = 1 }
	e.open()
	addDay(t, e.db, "2026-11-19")
	// 21 minutes credited while the server is down.
	e.srv.set(func(f *fakeServer) { f.status = http.StatusServiceUnavailable })
	first := time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)
	for i := range maxBatchesPerRun + 1 {
		e.credit(first.Add(time.Duration(i)*time.Minute), "app", 60)
	}
	e.run()
	if s := e.status(); s.Minutes.Waiting != maxBatchesPerRun+1 {
		t.Fatalf("waiting = %d, want %d", s.Minutes.Waiting, maxBatchesPerRun+1)
	}

	// Back up after the 19th has ended plus its grace hour.
	e.srv.set(func(f *fakeServer) { f.status = 0 })
	e.mu.Lock()
	e.now = time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC)
	e.mu.Unlock()
	e.run()
	id := e.deviceID()
	if days := e.srv.storedDays(id); len(days) != 0 {
		t.Fatalf("day sealed with %d minutes still waiting: %+v", e.status().Minutes.Waiting, days)
	}
	e.advance(time.Minute)
	e.run()
	for i := range maxBatchesPerRun + 1 {
		e.wantServerMinute(first.Add(time.Duration(i)*time.Minute), "app", 60)
	}
	if days := e.srv.storedDays(id); len(days) != 1 || days[0].Day != "2026-11-19" {
		t.Fatalf("server days = %+v, want 2026-11-19", days)
	}
	if s := e.status(); s.Minutes.Rejected != 0 || s.Minutes.Waiting != 0 {
		t.Fatalf("status = %+v", s.Minutes)
	}
}

// A minute refused because its day is already sealed can't be retried; its
// credit is lost, so it is shown in status as an error, not dropped silently.
func TestSealedDayRejectionShownInStatus(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC))
	e.run() // registers the device
	id := e.deviceID()
	e.srv.set(func(f *fakeServer) { f.days[id] = map[string]storedDay{"2026-11-20": {Day: "2026-11-20"}} })
	e.credit(time.Date(2026, 11, 20, 0, 59, 0, 0, time.UTC), "app", 60)
	e.advance(time.Minute)
	e.run()
	s := e.status()
	if s.Minutes.SealedError == "" || s.Minutes.SealedRejected != 1 || s.Minutes.Rejected != 1 || s.Minutes.Waiting != 0 {
		t.Fatalf("status = %+v, want the sealed-day rejection shown", s.Minutes)
	}
}

// The engine must never write a minute into a day that may be sealed: its
// window has to end before a day becomes sealable.
func TestSealableDaysAreOutsideTheWindow(t *testing.T) {
	if DayGrace < Window {
		t.Fatalf("DayGrace %v < Window %v: the engine could write minutes into a sealable day", DayGrace, Window)
	}
	if DayGrace < FinalAfter {
		t.Fatalf("DayGrace %v < FinalAfter %v: a sealable day could still have open minutes", DayGrace, FinalAfter)
	}
}

// While the server refuses minute batches as a whole, the minutes are held,
// and no day is sealed: sealing would strand them.
func TestNoSealingWhileMinutesOnHold(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 19, 23, 30, 5, 0, time.UTC))
	addDay(t, e.db, "2026-11-19")
	e.srv.set(func(f *fakeServer) { f.rulesVersion = "v-next" })
	minute := time.Date(2026, 11, 19, 23, 10, 0, 0, time.UTC)
	e.credit(minute, "app", 60)
	e.run()
	if s := e.status(); s.Minutes.Refused == "" || s.Minutes.Waiting != 1 {
		t.Fatalf("status after refusal = %+v", s.Minutes)
	}
	e.mu.Lock()
	e.now = time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC)
	e.mu.Unlock()
	e.run() // the hold has ended, but the server still refuses the batch
	id := e.deviceID()
	if days := e.srv.storedDays(id); len(days) != 0 {
		t.Fatalf("day sealed while its minutes were refused: %+v", days)
	}
	// The server learns the rules version: once the hold ends, the minute
	// goes up, then the day.
	e.srv.set(func(f *fakeServer) { f.rulesVersion = struggle.RulesVersion })
	e.advance(10 * time.Minute)
	e.run() // still on hold
	if days := e.srv.storedDays(id); len(days) != 0 {
		t.Fatalf("day sealed while minutes were on hold: %+v", days)
	}
	e.advance(31 * time.Minute)
	e.run()
	e.wantServerMinute(minute, "app", 60)
	if days := e.srv.storedDays(id); len(days) != 1 {
		t.Fatalf("server days = %+v, want the 19th", days)
	}
}
