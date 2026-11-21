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
