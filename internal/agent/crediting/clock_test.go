package crediting

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// logCounter counts log records whose message contains a word.
type logCounter struct {
	mu   sync.Mutex
	word string
	n    int
}

func (c *logCounter) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCounter) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCounter) WithGroup(string) slog.Handler            { return c }
func (c *logCounter) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, c.word) {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return nil
}

func (c *logCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (e *env) set(now time.Time) {
	e.mu.Lock()
	e.now = now
	e.mu.Unlock()
}

func (e *env) computeCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inputs)
}

// Nov 19 is sealed at Nov 20 01:00; then the clock goes back to Nov 19
// 23:31. Crediting pauses (no new minute is computed in the sealed day, so
// none is uploaded and refused as sealed), says why once, and resumes when
// the clock passes the sealed day, never writing into it.
func TestClockBehindSealedDayPausesCrediting(t *testing.T) {
	logs := &logCounter{word: "clock"}
	e := newEnv(t, time.Date(2026, 11, 19, 8, 0, 5, 0, time.UTC))
	e.options = func(o *Options) { o.Logger = slog.New(logs) }
	e.open()
	addDay(t, e.db, "2026-11-19")
	e.run() // registers the device on the 19th
	e.set(time.Date(2026, 11, 20, 1, 0, 5, 0, time.UTC))
	e.run()
	id := e.deviceID()
	if days := e.srv.storedDays(id); len(days) != 1 || days[0].Day != "2026-11-19" {
		t.Fatalf("server days = %+v, want the 19th sealed", days)
	}

	// The clock goes back into the sealed day.
	e.set(time.Date(2026, 11, 19, 23, 31, 5, 0, time.UTC))
	e.credit(time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC), "app", 60)
	calls, requests := e.computeCalls(), e.srv.requestCount()
	for range 3 {
		e.run()
		e.advance(time.Minute)
	}
	s := e.status()
	if s.Minutes.SealedRejected != 0 || s.Minutes.Rejected != 0 || s.Minutes.Waiting != 0 {
		t.Fatalf("minutes status = %+v, want nothing computed into the sealed day", s.Minutes)
	}
	if !strings.Contains(s.Minutes.Clock, "behind") {
		t.Fatalf("status clock = %q, want the clock shown as behind the sealed day", s.Minutes.Clock)
	}
	if e.computeCalls() != calls || e.srv.requestCount() != requests {
		t.Fatal("a paused runner computed or sent requests")
	}
	if n := logs.count(); n != 1 {
		t.Fatalf("%d log lines about the clock, want 1", n)
	}

	// The clock catches up a little past the sealed day: crediting resumes,
	// and minutes of the sealed day in the window are left alone.
	e.set(time.Date(2026, 11, 20, 0, 10, 5, 0, time.UTC))
	e.credit(time.Date(2026, 11, 19, 23, 50, 0, 0, time.UTC), "app", 60)
	e.credit(time.Date(2026, 11, 20, 0, 5, 0, 0, time.UTC), "app", 60)
	e.run()
	s = e.status()
	if s.Minutes.Clock != "" || s.Minutes.SealedRejected != 0 || s.Minutes.Rejected != 0 || s.Minutes.Waiting != 0 {
		t.Fatalf("status after the clock caught up = %+v", s.Minutes)
	}
	e.wantServerMinute(time.Date(2026, 11, 20, 0, 5, 0, 0, time.UTC), "app", 60)
	for _, m := range []time.Time{time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC), time.Date(2026, 11, 19, 23, 50, 0, 0, time.UTC)} {
		if _, ok := e.srv.minute(id, m, "app"); ok {
			t.Fatalf("minute %s of the sealed day was sent", m.Format(time.RFC3339))
		}
	}
	if n := logs.count(); n != 2 {
		t.Fatalf("%d log lines about the clock, want 2 (paused, resumed)", n)
	}
}

// The clock jumps five days ahead and comes back. While ahead, days are
// not sealed by the wrong clock: sealing goes by the server's clock too, so
// when the clock comes back nothing is paused and today's minutes are
// accepted.
func TestClockJumpAheadSealsNothing(t *testing.T) {
	start := time.Date(2026, 11, 20, 10, 0, 5, 0, time.UTC)
	e := newEnv(t, start)
	addDay(t, e.db, "2026-11-20")
	e.credit(time.Date(2026, 11, 20, 9, 59, 0, 0, time.UTC), "app", 60)
	e.run()
	id := e.deviceID()

	// Five days ahead by the agent's clock; the server's is right.
	ahead := 5 * 24 * time.Hour
	e.set(start.Add(ahead))
	e.srv.set(func(f *fakeServer) { f.offset = -ahead })
	for _, day := range []string{"2026-11-21", "2026-11-22", "2026-11-23", "2026-11-24"} {
		addDay(t, e.db, day)
	}
	e.credit(start.Add(ahead-5*time.Second-time.Minute), "app", 60)
	e.run()
	e.advance(time.Minute)
	e.run()
	if days := e.srv.storedDays(id); len(days) != 0 {
		t.Fatalf("days sealed by a clock five days ahead: %+v", days)
	}
	if s := e.status(); !strings.Contains(s.Minutes.Clock, "server") {
		t.Fatalf("status clock = %q, want the difference with the server shown", s.Minutes.Clock)
	}

	// Back to the right time.
	e.set(start.Add(10 * time.Minute))
	e.srv.set(func(f *fakeServer) { f.offset = 0 })
	e.credit(time.Date(2026, 11, 20, 10, 9, 0, 0, time.UTC), "app", 60)
	e.run()
	e.wantServerMinute(time.Date(2026, 11, 20, 10, 9, 0, 0, time.UTC), "app", 60)
	s := e.status()
	if s.Minutes.Clock != "" || s.Minutes.SealedRejected != 0 {
		t.Fatalf("status after the clock came back = %+v", s.Minutes)
	}
	if days := e.srv.storedDays(id); len(days) != 0 {
		t.Fatalf("server days = %+v", days)
	}

	// When the 20th really ends, it is sealed as usual.
	e.set(time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC))
	e.run()
	if days := e.srv.storedDays(id); len(days) != 1 || days[0].Day != "2026-11-20" {
		t.Fatalf("server days = %+v, want the 20th", days)
	}
}
