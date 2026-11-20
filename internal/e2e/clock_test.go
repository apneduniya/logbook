package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
)

// runAny runs the pipeline once without checking its status.
func (r *rig) runAny() {
	r.runner.RunOnce(context.Background())
}

func (r *rig) status() crediting.Status {
	r.t.Helper()
	s, err := crediting.LoadStatus(context.Background(), r.db)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// sealedDays returns the days the server stored for the device, oldest
// first.
func (r *rig) sealedDays() []string {
	r.t.Helper()
	rows, err := r.st.Pool.Query(context.Background(),
		`SELECT to_char(day, 'YYYY-MM-DD') FROM evidence_days WHERE device_id = $1 ORDER BY day`, r.device)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// wantNothingSealed checks that no day was sealed, locally or on the server,
// and that the clock disagreement is shown in status.
func (r *rig) wantNothingSealed(why string) {
	r.t.Helper()
	if days := r.sealedDays(); len(days) != 0 {
		r.t.Fatalf("%s: server holds sealed days %v", why, days)
	}
	if v, err := r.db.Get(context.Background(), "crediting.sealed_through"); err != nil || v != "" {
		r.t.Fatalf("%s: sealed_through = %q (%v), want unset", why, v, err)
	}
	if v, err := r.db.Get(context.Background(), "evidence.cursor"); err != nil || v != "" {
		r.t.Fatalf("%s: evidence cursor = %q (%v), want unset", why, v, err)
	}
	if s := r.status(); s.Minutes.Clock == "" {
		r.t.Fatalf("%s: status shows no clock problem", why)
	}
}

// The real time is Nov 20 12:01 and the agent's clock jumps five days
// ahead. A proxy between agent and server strips the Date header, or puts
// a wrong one in the future. The agent must not seal Nov 20 (still running)
// nor skip Nov 21-24 as empty: if it did, correcting the clock would pause
// crediting until Nov 25. It reads the server's time from the response body
// instead, sees the clocks disagree, seals nothing and keeps crediting.
func TestClockAheadBehindProxySealsNothing(t *testing.T) {
	for _, date := range []string{"strip", "future"} {
		t.Run(date, func(t *testing.T) {
			start := minute.Add(time.Minute + 5*time.Second) // Nov 20 12:01:05
			r := newRig(t, start)
			r.present(minute.Add(-5*time.Minute), minute.Add(10*time.Minute))
			r.edit(minute.Add(20*time.Second), "app")
			r.run()

			ahead := 5 * 24 * time.Hour
			r.with(func(r *rig) {
				r.date = date
				r.now = start.Add(time.Minute + ahead)
				r.serverOffset = -ahead
			})
			r.runAny()
			r.runAny()
			r.wantNothingSealed("clock five days ahead")

			// The clock is corrected: crediting goes on at once.
			r.with(func(r *rig) {
				r.now = minute.Add(4*time.Minute + 5*time.Second)
				r.serverOffset = 0
			})
			r.edit(minute.Add(3*time.Minute+10*time.Second), "app")
			r.run()
			if s := r.status(); s.Minutes.Clock != "" {
				t.Fatalf("status clock after the correction = %q", s.Minutes.Clock)
			}
			if _, total := r.serverRows(minute.Add(3 * time.Minute)); total != 60 {
				t.Fatalf("12:03 holds %d s after the clock was corrected, want 60", total)
			}

			// When Nov 20 has really ended, it is sealed.
			r.set(time.Date(2026, 11, 21, 1, 0, 5, 0, time.UTC))
			r.run()
			if days := r.sealedDays(); !slices.Equal(days, []string{"2026-11-20"}) {
				t.Fatalf("server days = %v, want [2026-11-20]", days)
			}
		})
	}
}

// The agent's clock is right; the server's is half an hour behind. Nov 20
// has ended plus the grace hour by both, but the clocks disagree, so
// nothing is sealed until they agree again.
func TestServerClockBehindSealsNothing(t *testing.T) {
	r := newRig(t, minute.Add(time.Minute+5*time.Second))
	r.present(minute.Add(-5*time.Minute), minute.Add(40*time.Minute))
	r.edit(minute.Add(20*time.Second), "app")
	r.run()
	r.set(minute.Add(40*time.Minute + 5*time.Second))
	r.run()

	r.with(func(r *rig) {
		r.now = time.Date(2026, 11, 21, 2, 0, 5, 0, time.UTC)
		r.serverOffset = -30 * time.Minute
	})
	r.runAny()
	r.wantNothingSealed("server clock 30 minutes behind")

	r.with(func(r *rig) { r.serverOffset = 0 })
	r.run()
	if days := r.sealedDays(); !slices.Equal(days, []string{"2026-11-20"}) {
		t.Fatalf("server days = %v, want [2026-11-20]", days)
	}
	if s := r.status(); s.Minutes.Clock != "" {
		t.Fatalf("status clock once the clocks agree = %q", s.Minutes.Clock)
	}
}

// The server is unreachable for three days. Nothing is sealed meanwhile and
// no minute is lost: once the server answers (and so the clocks can be
// compared), every minute goes up and the days are sealed in order.
func TestServerDownForDaysCatchesUp(t *testing.T) {
	r := newRig(t, minute.Add(time.Minute+5*time.Second))
	r.with(func(r *rig) { r.down = true })
	var minutes []time.Time
	for d := range 4 {
		m := minute.AddDate(0, 0, d)
		minutes = append(minutes, m)
		r.present(m.Add(-5*time.Minute), m.Add(40*time.Minute))
		r.edit(m.Add(20*time.Second), "app")
		r.set(m.Add(time.Minute + 5*time.Second))
		r.runAny()
		r.set(m.Add(40*time.Minute + 5*time.Second))
		r.runAny()
		r.set(m.Add(13*time.Hour + 5*time.Second)) // the next day, past the grace hour
		r.runAny()
		if days := r.sealedDays(); len(days) != 0 {
			t.Fatalf("days sealed while the server was down: %v", days)
		}
		if v, _ := r.db.Get(context.Background(), "crediting.sealed_through"); v != "" {
			t.Fatalf("sealed_through = %q while the server was down", v)
		}
	}

	// Back an hour later, past any backoff.
	r.with(func(r *rig) {
		r.down = false
		r.now = r.now.Add(time.Hour)
	})
	r.run()
	r.set(r.clock().Add(time.Minute))
	r.run()
	for _, m := range minutes {
		if _, total := r.serverRows(m); total != 60 {
			t.Fatalf("%s holds %d s after catching up, want 60", m.Format(time.DateTime), total)
		}
		r.wantServerMatchesEngine(m)
	}
	want := []string{"2026-11-20", "2026-11-21", "2026-11-22", "2026-11-23"}
	if days := r.sealedDays(); !slices.Equal(days, want) {
		t.Fatalf("server days = %v, want %v", days, want)
	}
	if s := r.status(); s.Minutes.Clock != "" || s.Minutes.Waiting != 0 || s.Evidence.Error != "" {
		t.Fatalf("status after catching up = %+v", s)
	}
}
