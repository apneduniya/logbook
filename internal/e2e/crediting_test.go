package e2e

import (
	"slices"
	"testing"
	"time"
)

// Nine projects with equal signals in an open minute: the engine credits
// the first eight. A late signal moves the ninth into the top eight and the
// eighth out. The server must take the replacement in the same upload that
// drops the displaced project, so it holds what the engine computed (60 s),
// not 60 s less the displaced share with the ninth refused.
func TestOpenMinuteTopProjectsReplaced(t *testing.T) {
	t.Parallel()
	r := newRig(t, minute.Add(time.Minute+5*time.Second))
	r.present(minute.Add(-5*time.Minute), minute.Add(10*time.Minute))
	r.edit(minute.Add(20*time.Second), "a", "b", "c", "d", "e", "f", "g", "h", "i")
	r.run()
	if got, _ := r.serverRows(minute); len(got) != 8 || slices.Contains(got, "i 8") || slices.Contains(got, "i 7") {
		t.Fatalf("first upload = %v, want a..h", got)
	}
	r.wantServerMatchesEngine(minute)

	// A late heartbeat for i, still in the rewrite window.
	r.edit(minute.Add(50*time.Second), "i")
	r.set(minute.Add(2*time.Minute + 5*time.Second))
	r.run()
	got, total := r.serverRows(minute)
	if total != 60 || !slices.ContainsFunc(got, func(s string) bool { return s[0] == 'i' }) ||
		slices.ContainsFunc(got, func(s string) bool { return s[0] == 'h' }) {
		t.Fatalf("after the late signal the server has %v (%d s), want i in place of h and 60 s", got, total)
	}
	r.wantServerMatchesEngine(minute)
	r.finish("2026-11-20")
}

// The top eight of an open minute change several times within the rewrite
// window. After every run the server holds exactly the engine's rows, never
// more than eight; once final, the minute is frozen.
func TestOpenMinuteTopProjectsChurn(t *testing.T) {
	t.Parallel()
	r := newRig(t, minute.Add(time.Minute+5*time.Second))
	r.present(minute.Add(-5*time.Minute), minute.Add(45*time.Minute))
	all := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	r.edit(minute.Add(10*time.Second), all...)
	r.run()
	r.wantServerMatchesEngine(minute)

	// Each late batch of signals lifts different projects into the top
	// eight, and some back out again.
	late := [][]string{{"l", "k"}, {"j", "j", "i"}, {"l", "l", "l", "a"}, {"h", "h", "h", "h"}, {"k", "k", "k", "k", "k"}}
	for i, ps := range late {
		at := minute.Add(time.Duration(20+5*i) * time.Second)
		for _, p := range ps {
			r.edit(at, p)
			at = at.Add(time.Second / 2)
		}
		r.set(minute.Add(time.Duration(3+4*i)*time.Minute + 5*time.Second))
		r.run()
		r.wantServerMatchesEngine(minute)
	}
	frozen, frozenTotal := r.serverRows(minute)
	if frozenTotal != 60 {
		t.Fatalf("minute holds %d s, want 60", frozenTotal)
	}

	// Final now: a late signal changes nothing on the server.
	r.edit(minute.Add(55*time.Second), "b", "b", "b", "b", "b", "b", "b", "b")
	r.set(minute.Add(31*time.Minute + 5*time.Second))
	r.run()
	r.set(minute.Add(32*time.Minute + 5*time.Second))
	r.run()
	if got, _ := r.serverRows(minute); !slices.Equal(got, frozen) {
		t.Fatalf("final minute changed on the server: %v, was %v", got, frozen)
	}
	r.finish("2026-11-20")
}

// The agent computed minutes of Nov 19 before the user signed in, and the
// device registered on Nov 20, so the server refuses them for good (before
// the day the device registered), without stored rows. Each is refused
// while open, sent once more when final, then dropped and counted: it is
// not retried forever, and Nov 20 is still sealed.
func TestMinutesBeforeRegistrationDayDropped(t *testing.T) {
	t.Parallel()
	night := time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)
	r := newRig(t, night.Add(5*time.Second), registeredOn(minute))
	runEvery := func(from, to time.Time) {
		for at := from; at.Before(to); at = at.Add(time.Minute) {
			r.set(at.Add(5 * time.Second))
			r.runAny()
		}
	}
	r.present(night.Add(-10*time.Minute), night.Add(15*time.Minute))
	for i := range 10 {
		r.edit(night.Add(time.Duration(i)*time.Minute+20*time.Second), "app")
	}
	runEvery(night.Add(time.Minute), night.Add(55*time.Minute))
	s := r.status()
	if s.Minutes.Rejected == 0 || s.Minutes.Waiting != 0 {
		t.Fatalf("status after Nov 19 = %+v, want its minutes refused and dropped", s.Minutes)
	}
	if rows, _ := r.db.StoredMinutes(t.Context(), night, night.Add(time.Hour)); len(rows) != 0 {
		t.Fatalf("Nov 19 rows kept: %+v", rows)
	}
	rejected := s.Minutes.Rejected

	r.present(m(-5), m(10))
	r.edit(m(0).Add(20*time.Second), "app")
	runEvery(m(1), m(45))
	if _, total := r.serverRows(m(0)); total != 60 {
		t.Fatalf("12:00 holds %d s, want 60", total)
	}
	runEvery(time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC), time.Date(2026, 11, 21, 1, 5, 0, 0, time.UTC))
	if days := r.sealedDays(); !slices.Equal(days, []string{"2026-11-20"}) {
		t.Fatalf("server days = %v, want [2026-11-20]; status %+v", days, r.status().Minutes)
	}
	if s := r.status(); s.Minutes.Rejected != rejected || s.Minutes.Waiting != 0 {
		t.Fatalf("status = %+v; refused minutes were retried (rejected was %d)", s.Minutes, rejected)
	}
	r.check()
}
