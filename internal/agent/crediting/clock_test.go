package crediting

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
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

	// The clock catches up past the sealed day: crediting resumes. Minutes
	// of the sealed day in the window are left alone, and so are the
	// minutes the run at 01:00 made final (up to 00:31), even empty ones.
	e.set(time.Date(2026, 11, 20, 0, 40, 5, 0, time.UTC))
	e.credit(time.Date(2026, 11, 19, 23, 50, 0, 0, time.UTC), "app", 60)
	e.credit(time.Date(2026, 11, 20, 0, 5, 0, 0, time.UTC), "app", 60)
	e.credit(time.Date(2026, 11, 20, 0, 35, 0, 0, time.UTC), "app", 60)
	e.run()
	s = e.status()
	if s.Minutes.Clock != "" || s.Minutes.SealedRejected != 0 || s.Minutes.Rejected != 0 || s.Minutes.Waiting != 0 {
		t.Fatalf("status after the clock caught up = %+v", s.Minutes)
	}
	e.wantServerMinute(time.Date(2026, 11, 20, 0, 35, 0, 0, time.UTC), "app", 60)
	for _, m := range []time.Time{time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC), time.Date(2026, 11, 19, 23, 50, 0, 0, time.UTC),
		time.Date(2026, 11, 20, 0, 5, 0, 0, time.UTC)} {
		if _, ok := e.srv.minute(id, m, "app"); ok {
			t.Fatalf("minute %s, sealed or final, was sent", m.Format(time.RFC3339))
		}
	}
	// Paused and resumed; final minutes stay final whatever the clock did
	// (ADR 0017), so nothing else is said.
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

// A day is sealed (or skipped as empty) only when the minutes response
// carries the server's time and it is within MaxClockSkew of the system
// clock. Otherwise nothing moves, status says why, minutes still go up,
// and the days are sealed once the server's time is usable again. The Date
// header plays no part: here it always holds the right time.
func TestSealingNeedsServerTime(t *testing.T) {
	tests := []struct {
		name       string
		serverTime string
		offset     time.Duration
		seal       bool
	}{
		{"missing", "-", 0, false},
		{"malformed", "yesterday", 0, false},
		{"days ahead", "2026-11-28T01:10:05Z", 0, false},
		{"far behind", "2026-11-19T01:10:05Z", 0, false},
		{"just behind", "", -(MaxClockSkew + time.Second), false},
		{"just ahead", "", MaxClockSkew + time.Second, false},
		{"agrees", "", 0, true},
		{"within tolerance behind", "", -MaxClockSkew + time.Second, true},
		{"within tolerance ahead", "", MaxClockSkew - time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, time.Date(2026, 11, 20, 10, 0, 5, 0, time.UTC))
			addDay(t, e.db, "2026-11-20")
			addDay(t, e.db, "2026-11-22")
			e.credit(time.Date(2026, 11, 20, 9, 59, 0, 0, time.UTC), "app", 60)
			e.run()
			id := e.deviceID()

			e.srv.set(func(f *fakeServer) { f.serverTime, f.offset = tt.serverTime, tt.offset })
			e.set(time.Date(2026, 11, 23, 1, 10, 5, 0, time.UTC))
			e.credit(time.Date(2026, 11, 23, 0, 39, 0, 0, time.UTC), "app", 60)
			e.run()
			days := e.srv.storedDays(id)
			if !tt.seal {
				if len(days) != 0 {
					t.Fatalf("days sealed: %+v", days)
				}
				for _, key := range []string{keySealedThrough, keyEvidenceCursor} {
					if v, err := e.db.Get(t.Context(), key); err != nil || v != "" {
						t.Fatalf("%s = %q (%v), want unset", key, v, err)
					}
				}
				if s := e.status(); s.Minutes.Clock == "" || s.Minutes.Waiting != 0 {
					t.Fatalf("minutes status = %+v, want the clock problem shown and minutes uploaded", s.Minutes)
				}
				e.wantServerMinute(time.Date(2026, 11, 23, 0, 39, 0, 0, time.UTC), "app", 60)

				// The server's time is usable again: the days are sealed in order.
				e.srv.set(func(f *fakeServer) { f.serverTime, f.offset = "", 0 })
				e.advance(time.Minute)
				e.run()
				days = e.srv.storedDays(id)
			}
			// The 20th's block holds every row logged so far; the 21st and
			// the 22nd have none left and are skipped.
			if len(days) != 1 || days[0].Day != "2026-11-20" || days[0].EventCounts[evidence.TypeHeartbeat] != 2 {
				t.Fatalf("server days = %+v, want the 20th with both days' rows", days)
			}
			if v, err := e.db.Get(t.Context(), keySealedThrough); err != nil || v != "2026-11-22" {
				t.Fatalf("sealed through %q (%v), want 2026-11-22", v, err)
			}
			if s := e.status(); s.Minutes.Clock != "" {
				t.Fatalf("status clock = %q once the clocks agree", s.Minutes.Clock)
			}
		})
	}
}
