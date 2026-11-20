package crediting

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// delivery is one raw event and when it reaches the agent.
type delivery struct {
	at time.Time
	ev wire.LocalEvent
	hb *wire.Heartbeat
}

// downtime is a span the agent is stopped: no runs, no sensor samples, and
// nothing reaches it (hooks spool, editors queue).
type downtime struct{ from, to time.Time }

func isDown(downs []downtime, t time.Time) bool {
	return slices.ContainsFunc(downs, func(d downtime) bool { return !t.Before(d.from) && t.Before(d.to) })
}

// randomStream makes two hours of activity, heartbeats and AI hook events
// with random delivery delays (most within seconds, some minutes, a few an
// hour, as from wakatime-cli's offline queue), and random downtimes. An
// event due while the agent is down arrives after it starts again: hook
// events with the startup spool drain, heartbeats when the editor next
// retries.
func randomStream(rng *rand.Rand, from, to time.Time) ([]delivery, []downtime) {
	var downs []downtime
	for range 1 + rng.IntN(2) {
		start := from.Add(time.Duration(10+rng.IntN(90)) * time.Minute)
		downs = append(downs, downtime{start, start.Add(time.Duration(3+rng.IntN(60)) * time.Minute)})
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
	add := func(t time.Time, ev wire.LocalEvent, hb *wire.Heartbeat, spooled bool) {
		delay := time.Duration(rng.IntN(20)) * time.Second
		switch k := rng.IntN(20); {
		case k == 0:
			delay = time.Duration(20+rng.IntN(50)) * time.Minute
		case k < 3:
			delay = time.Duration(1+rng.IntN(10)) * time.Minute
		}
		at := t.Add(delay)
		if r := restart(at); !r.Equal(at) {
			at = r.Add(time.Duration(rng.IntN(300)) * time.Second)
			if spooled {
				at = r.Add(time.Second) // the startup drain
			}
		}
		out = append(out, delivery{at: at, ev: ev, hb: hb})
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
			add(t, wire.LocalEvent{}, &h, false)
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
			add(t, wire.LocalEvent{Type: "ai", AI: &e}, nil, true)
		}
	}
	slices.SortStableFunc(out, func(a, b delivery) int { return a.at.Compare(b.at) })
	return out, downs
}

// finalCredit is the stored final minutes before the watermark, by minute.
func finalCredit(t *testing.T, db *localdb.DB, from time.Time) (map[time.Time]string, time.Time) {
	t.Helper()
	ctx := context.Background()
	ft, err := db.FinalThrough(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.StoredMinutes(ctx, from, ft)
	if err != nil {
		t.Fatal(err)
	}
	out := map[time.Time]string{}
	for _, r := range rows {
		out[r.Minute] += creditOf(r)
	}
	return out, ft
}

// creditOf is what a re-run is compared on: project, label, credit, cuts.
// A removal row (sent to the server for a displaced project) says nothing.
func creditOf(r localdb.CreditedMinute) string {
	if r.Label == string(struggle.LabelIdle) && r.Credited == 0 && len(r.Cuts) == 0 && len(r.Flags) == 0 {
		return ""
	}
	return fmt.Sprintf("[%s %s %d %v]", r.Project, r.Label, r.Credited, r.Cuts)
}

// Random event streams, delivered with random delays, with the agent
// stopped and started again at random: every minute that is final never
// changes afterwards, and a replay of the raw events with the persisted
// cutoffs gives every final minute exactly its stored credit.
func TestFinalMinutesNeverChangeAndReplay(t *testing.T) {
	for seed := range uint64(12) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 16))
			start := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
			end := start.Add(2 * time.Hour)
			stream, downs := randomStream(rng, start, end)
			settle := []time.Duration{-1, 3 * time.Minute, DefaultSettle}[rng.IntN(3)]

			e := newEnv(t, start.Add(5*time.Second))
			drained := make(chan struct{})
			close(drained)
			e.options = func(o *Options) {
				o.Compute, o.Settle, o.Drained = struggle.Compute, settle, drained
			}
			e.open()

			seen := map[time.Time]string{}
			next, wasDown := 0, false
			for at := start; at.Before(end.Add(time.Hour)); at = at.Add(time.Minute) {
				now := at.Add(5 * time.Second)
				e.set(now)
				if isDown(downs, now) {
					wasDown = true
					continue
				}
				if wasDown { // a restart: the spool drains before anything is final
					wasDown = false
					drained = make(chan struct{})
					e.open()
				}
				for ; next < len(stream) && !stream[next].at.After(now); next++ {
					d := stream[next]
					e.mu.Lock()
					e.arrived = d.at
					e.mu.Unlock()
					var err error
					if d.hb != nil {
						err = e.db.Enqueue(context.Background(), []wire.Heartbeat{*d.hb}, false)
					} else {
						err = e.db.InsertEvents(context.Background(), []wire.LocalEvent{d.ev})
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				e.mu.Lock()
				e.arrived = time.Time{}
				e.mu.Unlock()
				select {
				case <-drained:
				default:
					close(drained)
				}
				e.run()

				final, _ := finalCredit(t, e.db, start.Add(-Window))
				for m, c := range seen {
					if final[m] != c {
						t.Fatalf("at %s the final minute %s changed from %q to %q", now.Format("15:04:05"),
							m.Format("15:04"), c, final[m])
					}
				}
				for m, c := range final {
					seen[m] = c
				}
			}

			ctx := context.Background()
			final, ft := finalCredit(t, e.db, start.Add(-Window))
			if ft.Before(end) {
				t.Fatalf("final only through %s", ft.Format("15:04"))
			}
			journal, err := e.db.RepoMap(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cutoffs, err := e.db.CutoffsBetween(ctx, start.Add(-Window), ft)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := ReplayMinutes(ctx, e.db, journal, cutoffs, start.Add(-Window), ft, struggle.Compute, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			want := map[time.Time]string{}
			for _, r := range replayed {
				want[r.Minute] += creditOf(r)
			}
			var minutes []time.Time
			for m := range want {
				minutes = append(minutes, m)
			}
			for m := range final {
				minutes = append(minutes, m)
			}
			slices.SortFunc(minutes, func(a, b time.Time) int { return cmp.Compare(a.Unix(), b.Unix()) })
			for _, m := range minutes {
				if final[m] != want[m] {
					t.Fatalf("minute %s: stored %q, replay with cutoff %s gives %q (downtimes %v)", m.Format("15:04"),
						final[m], CutoffAt(cutoffs, m).Format("15:04"), want[m], downs)
				}
			}
		})
	}
}
