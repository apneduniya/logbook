package crediting

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// delivery is one raw event and when it reaches the agent (real time).
type delivery struct {
	at  time.Time
	ev  wire.LocalEvent
	hbs []wire.Heartbeat
}

// downtime is a span the agent doesn't run: stopped (a restart follows:
// the database is reopened and the spool drained) or asleep (the process
// keeps its state; the machine was suspended). Nothing reaches it
// meanwhile: hooks spool, editors queue.
type downtime struct {
	from, to time.Time
	asleep   bool
}

func (d downtime) String() string {
	kind := "stopped"
	if d.asleep {
		kind = "asleep"
	}
	return fmt.Sprintf("%s %s-%s", kind, d.from.Format("15:04"), d.to.Format("15:04"))
}

func isDown(downs []downtime, t time.Time) bool {
	return slices.ContainsFunc(downs, func(d downtime) bool { return !t.Before(d.from) && t.Before(d.to) })
}

// skew is a span the agent's clock is off by off (forward or back).
type skew struct {
	from, to time.Time
	off      time.Duration
}

func (s skew) String() string {
	return fmt.Sprintf("%+v %s-%s", s.off, s.from.Format("15:04"), s.to.Format("15:04"))
}

func skewAt(skews []skew, t time.Time) time.Duration {
	for _, s := range skews {
		if !t.Before(s.from) && t.Before(s.to) {
			return s.off
		}
	}
	return 0
}

// randomStream makes two hours of activity, heartbeats, AI hook events and
// git commits with random delivery delays (most within seconds, some
// minutes, a few an hour, as from wakatime-cli's offline queue), random
// downtimes and random clock jumps. An event due while the agent is down
// arrives after it runs again: hook events with the startup spool drain,
// heartbeats when the editor next retries (sometimes resent: a duplicate).
func randomStream(rng *rand.Rand, from, to time.Time) ([]delivery, []downtime, []skew) {
	var downs []downtime
	for range 1 + rng.IntN(2) {
		start := from.Add(time.Duration(10+rng.IntN(90)) * time.Minute)
		downs = append(downs, downtime{start, start.Add(time.Duration(3+rng.IntN(60)) * time.Minute), rng.IntN(2) == 0})
	}
	var skews []skew
	for range rng.IntN(3) {
		start := from.Add(time.Duration(rng.IntN(100)) * time.Minute)
		off := time.Duration(2+rng.IntN(40)) * time.Minute
		if rng.IntN(2) == 0 {
			off = -off
		}
		skews = append(skews, skew{start, start.Add(time.Duration(5+rng.IntN(25)) * time.Minute), off})
	}
	restart := func(t time.Time) time.Time {
		for _, d := range downs {
			if !t.Before(d.from) && t.Before(d.to) {
				return d.to
			}
		}
		return t
	}
	var out []delivery
	add := func(t time.Time, d delivery, spooled bool) {
		delay := time.Duration(rng.IntN(20)) * time.Second
		switch k := rng.IntN(20); {
		case k == 0:
			delay = time.Duration(20+rng.IntN(50)) * time.Minute
		case k < 3:
			delay = time.Duration(1+rng.IntN(10)) * time.Minute
		}
		d.at = t.Add(delay)
		if r := restart(d.at); !r.Equal(d.at) {
			d.at = r.Add(time.Duration(rng.IntN(300)) * time.Second)
			if spooled {
				d.at = r.Add(time.Second) // the startup drain
			}
		}
		out = append(out, d)
	}
	hashes := []string{strings.Repeat("a1", 32), strings.Repeat("b2", 32)}
	kinds := []wire.AIEventKind{wire.AIToolStart, wire.AIToolEnd, wire.AIToolEnd, wire.AITurnEnd, wire.AIPermissionAsk}
	present := rng.IntN(2) == 0
	for t := from; t.Before(to); t = t.Add(10 * time.Second) {
		if t.Second() == 0 && t.Minute()%5 == 0 && rng.IntN(3) == 0 {
			present = !present
		}
		if present && !isDown(downs, t) { // the sensor runs in the agent
			focus := []string{"terminal", "editor", "ai", "browser"}[rng.IntN(4)]
			s := wire.ActivitySample{Time: t, IdleSeconds: float64(rng.IntN(90)), FocusKind: focus}
			out = append(out, delivery{at: t.Add(time.Duration(rng.IntN(3)) * time.Second),
				ev: wire.LocalEvent{Type: "activity", Activity: &s}})
		}
		if rng.IntN(6) == 0 {
			p := []string{"app", "lib"}[rng.IntN(2)]
			h := wire.Heartbeat{Entity: "/src/" + p + "/main.go", Project: p, IsWrite: rng.IntN(3) == 0,
				Lineno: new(1 + rng.IntN(3)), Time: float64(t.Add(time.Duration(rng.IntN(10))*time.Second).UnixMilli()) / 1000}
			hbs := []wire.Heartbeat{h}
			if rng.IntN(8) == 0 {
				hbs = append(hbs, h) // resent by wakatime-cli: stored once
			}
			add(t, delivery{hbs: hbs}, false)
		}
		if rng.IntN(9) == 0 {
			e := wire.AIEvent{Time: t.Add(time.Duration(rng.IntN(10)) * time.Second), Harness: wire.HarnessClaudeCode,
				SessionID: "cc", Cwd: "/src/app"}
			if rng.IntN(3) == 0 {
				e.Kind, e.PromptChars, e.PromptHash = wire.AIPromptSubmit, 60, hashes[rng.IntN(2)]
				if rng.IntN(4) == 0 {
					e.PromptChars, e.PromptHash = 3, "c3" // "yes"
				}
			} else {
				e.Kind = kinds[rng.IntN(len(kinds))]
			}
			add(t, delivery{ev: wire.LocalEvent{Type: "ai", AI: &e}}, true)
		}
		if rng.IntN(120) == 0 {
			g := wire.GitEvent{Time: t, Project: "app", Kind: "commit", CommitHash: fmt.Sprintf("%040x", rng.IntN(4)), FilesChanged: 1}
			add(t, delivery{ev: wire.LocalEvent{Type: "git", Git: &g}}, false) // a commit seen twice is stored once
		}
	}
	slices.SortStableFunc(out, func(a, b delivery) int { return a.at.Compare(b.at) })
	return out, downs, skews
}

// creditOf is what a re-run is compared on: project, label, credit, cuts.
func creditOf(r localdb.CreditedMinute) string {
	return fmt.Sprintf("[%s %s %d %v]", r.Project, r.Label, r.Credited, r.Cuts)
}

// settledMinutes returns, by minute, the stored minutes whose rows are all
// final and acknowledged: agreed with the server. Each value has the rows'
// content and keys.
func settledMinutes(t *testing.T, db *localdb.DB, from, to time.Time) map[time.Time]string {
	t.Helper()
	rows, err := db.StoredMinutes(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	out, open := map[time.Time]string{}, map[time.Time]bool{}
	for _, r := range rows {
		if !r.Final || !r.Acknowledged {
			open[r.Minute] = true
		}
		out[r.Minute] += fmt.Sprintf("%s %v %v@%s/%d", creditOf(r), r.Flags, r.RulesVersion, r.Key.T.Format("15:04"), r.Key.H)
	}
	for m := range open {
		delete(out, m)
	}
	return out
}

// The property test of ADR 0017. Random interleavings of event arrival
// (late, duplicated, spooled), git commits, clock jumps forward and back,
// suspends, restarts, failed requests and lost responses, with the real
// engine, local database, API client and a fake server following the
// server contract. Then, once things calm down:
//
//   - a minute agreed with the server (final and acknowledged) never
//     changes afterwards;
//   - every final minute holds exactly the server's rows, keys included,
//     and the server holds no row the agent doesn't;
//   - ReplayMinute at each final minute's key reproduces its label,
//     project, credit and cuts;
//   - ReplayBlock reproduces the signed content hash of the sealed day.
func TestFinalMinutesAgreeWithServerAndReplay(t *testing.T) {
	var (
		mu            sync.Mutex
		finalRefusals int
	)
	t.Cleanup(func() {
		// Across seeds the server must have refused a final rewrite with
		// its rows at least once, or adoption went untested.
		if !t.Failed() && finalRefusals == 0 {
			t.Error("no seed made the server answer a final refusal with its rows")
		}
	})
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rng := rand.New(rand.NewPCG(seed, 17))
			start := time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC)
			end := start.Add(2 * time.Hour)
			calm := time.Date(2026, 11, 20, 1, 15, 0, 0, time.UTC) // the 19th sealable from 01:00
			stream, downs, skews := randomStream(rng, start, end)
			settle := []time.Duration{-1, 3 * time.Minute, DefaultSettle}[rng.IntN(3)]

			e := newEnv(t, start.Add(5*time.Second))
			drained := make(chan struct{})
			close(drained)
			e.options = func(o *Options) {
				o.Compute, o.Settle, o.Drained = struggle.Compute, settle, drained
			}
			e.open()

			agreed := map[time.Time]string{}
			next, down := 0, downtime{}
			for rt := start; rt.Before(calm); rt = rt.Add(time.Minute) {
				now := rt.Add(5 * time.Second)
				off := skewAt(skews, now)
				e.set(now.Add(off))
				e.srv.set(func(f *fakeServer) { f.offset = -off })
				if isDown(downs, now) {
					for _, d := range downs {
						if !now.Before(d.from) && now.Before(d.to) {
							down = d
						}
					}
					continue
				}
				if !down.from.IsZero() {
					if !down.asleep { // a restart: the spool drains before anything is final
						drained = make(chan struct{})
						e.open()
					}
					down = downtime{}
				}
				for ; next < len(stream) && !stream[next].at.After(now); next++ {
					d := stream[next]
					var err error
					if d.hbs != nil {
						err = e.db.Enqueue(ctx, d.hbs, false)
					} else {
						err = e.db.InsertEvents(ctx, []wire.LocalEvent{d.ev})
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-drained:
				default:
					close(drained)
				}
				failing := rt.Before(end) && rng.IntN(12) == 0
				lost := rt.Before(end) && !failing && rng.IntN(12) == 0
				e.srv.set(func(f *fakeServer) {
					f.status, f.dropResponse = 0, lost
					if failing {
						f.status = 503
					}
				})
				e.run()

				now2 := settledMinutes(t, e.db, start.Add(-3*time.Hour), calm)
				for m, v := range agreed {
					if now2[m] != v {
						t.Fatalf("at %s the agreed minute %s changed from %q to %q", now.Format("15:04"), m.Format("15:04"), v, now2[m])
					}
				}
				for m, v := range now2 {
					agreed[m] = v
				}
			}

			e.srv.set(func(f *fakeServer) {
				mu.Lock()
				finalRefusals += f.finalRefusals
				mu.Unlock()
				st := e.status().Minutes
				t.Logf("settle %v, downtimes %v, skews %v: %d final refusals, %d other refusals, %d late events, %d agreed minutes",
					settle, downs, skews, f.finalRefusals, st.Rejected, st.LateIgnored, len(agreed))
			})

			// Every stored minute old enough to be final is final and agreed,
			// and matches the server.
			from, to := start.Add(-time.Hour), calm.Add(-35*time.Minute)
			local, err := e.db.StoredMinutes(ctx, from, to)
			if err != nil {
				t.Fatal(err)
			}
			id := e.deviceID()
			byMinute := map[time.Time][]localdb.CreditedMinute{}
			for _, r := range local {
				if !r.Final || !r.Acknowledged {
					t.Fatalf("minute %s not settled at the end: %+v (downtimes %v, skews %v)", r.Minute.Format("15:04"), r, downs, skews)
				}
				byMinute[r.Minute] = append(byMinute[r.Minute], r)
			}
			held := e.srv.heldBetween(id, from, to)
			if len(held) != len(local) {
				t.Fatalf("server holds %d rows, the agent %d", len(held), len(local))
			}
			for _, r := range local {
				s, ok := e.srv.minute(id, r.Minute, r.Project)
				if !ok || s.Label != r.Label || s.Credited != r.Credited || !slices.Equal(s.Cuts, r.Cuts) ||
					!slices.Equal(s.Flags, r.Flags) || !s.ComputedAt.Equal(r.Key.T) || s.Snapshot == nil || *s.Snapshot != r.Key.H {
					t.Fatalf("minute %s %s: agent %+v, server %+v (exists %v)", r.Minute.Format("15:04"), r.Project, r, s, ok)
				}
			}

			// Every final minute replays at its key.
			for m, rows := range byMinute {
				key := rows[0].Key
				for _, r := range rows {
					if r.Key != key {
						t.Fatalf("minute %s mixes keys: %+v", m.Format("15:04"), rows)
					}
				}
				replayed, err := ReplayMinute(ctx, e.db, m, key, struggle.Compute, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := describe(replayed), describe(rows); got != want {
					t.Fatalf("minute %s at key %s/%d: stored %s, replay %s", m.Format("15:04"), key.T.Format("15:04"), key.H, want, got)
				}
			}

			// The 19th is sealed, and its block re-hashes to the signed hash.
			day, ok, err := e.db.EvidenceDayOf(ctx, id, "2026-11-19")
			if err != nil || !ok || day.State != localdb.EvidenceAccepted {
				t.Fatalf("evidence for the 19th = %+v, %v, %v", day, ok, err)
			}
			rows, err := ReplayBlock(ctx, e.db, day)
			if err != nil {
				t.Fatal(err)
			}
			if h, err := evidence.ContentHash(rows); err != nil || h != day.ContentHash {
				t.Fatalf("replayed block hash = %s, %v; signed %s", h, err, day.ContentHash)
			}
			_, dayEnd, _ := evidence.DayBounds(day.Day)
			for _, r := range local {
				if r.Minute.Before(dayEnd) && r.Key.H > day.SeqTo {
					t.Fatalf("minute %s of the sealed day has key H %d past the block's end %d", r.Minute.Format("15:04"), r.Key.H, day.SeqTo)
				}
			}
		})
	}
}

// describe is rows compared on project, label, credit and cuts, in project
// order.
func describe(rows []localdb.CreditedMinute) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = creditOf(r)
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}
