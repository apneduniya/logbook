package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/server/credited"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// rig is the agent's real crediting pipeline (local database, real engine,
// HTTP client) talking to the server's real minutes endpoint, service and
// Postgres store, on one shared clock.
type rig struct {
	t      *testing.T
	st     *store.Store
	db     *localdb.DB
	runner *crediting.Runner
	device uuid.UUID

	mu  sync.Mutex
	now time.Time
	// computed is the engine's last output per minute, for comparison.
	computed map[time.Time][]struggle.Minute
}

// identity is a registered device that never changes.
type identity struct{ dev devicekey.Device }

func (i identity) Ensure(context.Context, serverapi.Target) (devicekey.Device, error) {
	return i.dev, nil
}
func (identity) MarkRevoked(context.Context, serverapi.Target) (bool, error) { return false, nil }
func (identity) Confirm(context.Context, serverapi.Target) error             { return nil }
func (identity) Forget(context.Context) error                                { return nil }

func newRig(t *testing.T, start time.Time) *rig {
	t.Helper()
	r := &rig{t: t, now: start, computed: map[time.Time][]struggle.Minute{}}
	r.st = storetest.New(t)
	user := seedtest.User(t, r.st, "alice", "UTC", true)
	registered := start.UTC().Truncate(24 * time.Hour)
	r.device = seedtest.DeviceAt(t, r.st, user, "laptop", registered)

	mux := http.NewServeMux()
	credited.NewHandler(credited.NewService(r.st, nil, credited.WithClock(r.clock)), httpapitest.StubSession).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Set(httpapitest.UserHeader, user.String())
		mux.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)

	db, err := localdb.Open(context.Background(), filepath.Join(t.TempDir(), "agent.db"), localdb.WithClock(r.clock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r.db = db
	r.runner = crediting.New(crediting.Options{
		Store: db, Server: serverapi.New(srv.Client(), "logbook-agent/e2e"),
		Identity: identity{devicekey.Device{ID: r.device.String(), RegisteredAt: registered}},
		Target: func() crediting.Target {
			return crediting.Target{Target: serverapi.Target{BaseURL: srv.URL, APIKey: "key"}}
		},
		Compute: r.compute, TZ: time.UTC, Now: r.clock,
		Jitter: func() float64 { return 0 },
	})
	return r
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *rig) set(now time.Time) {
	r.mu.Lock()
	r.now = now
	r.mu.Unlock()
}

// compute is the real engine, remembering what it said.
func (r *rig) compute(in struggle.Input) struggle.Result {
	res := struggle.Compute(in)
	r.mu.Lock()
	defer r.mu.Unlock()
	byMinute := map[time.Time][]struggle.Minute{}
	for _, m := range res.Minutes {
		byMinute[m.Minute] = append(byMinute[m.Minute], m)
	}
	for m, rows := range byMinute {
		r.computed[m] = rows
	}
	return res
}

func (r *rig) run() {
	r.t.Helper()
	r.runner.RunOnce(context.Background())
	s, err := crediting.LoadStatus(context.Background(), r.db)
	if err != nil {
		r.t.Fatal(err)
	}
	if s.Minutes.Rejected != 0 || s.Minutes.LastError != "" || s.Minutes.Refused != "" {
		r.t.Fatalf("after run at %s: minutes status %+v", r.clock().Format("15:04:05"), s.Minutes)
	}
}

// present records the person at the editor from from to to: an activity
// sample every 10 seconds with fresh input.
func (r *rig) present(from, to time.Time) {
	r.t.Helper()
	var evs []wire.LocalEvent
	for t := from; t.Before(to); t = t.Add(10 * time.Second) {
		evs = append(evs, wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{Time: t, IdleSeconds: 1, FocusKind: "editor"}})
	}
	if err := r.db.InsertEvents(context.Background(), evs); err != nil {
		r.t.Fatal(err)
	}
}

// edit records one editor heartbeat per project at t (a write, so it is the
// person's own signal).
func (r *rig) edit(t time.Time, projects ...string) {
	r.t.Helper()
	var hbs []wire.Heartbeat
	for i, p := range projects {
		hbs = append(hbs, wire.Heartbeat{Entity: fmt.Sprintf("/src/%s/main.go", p), Project: p, IsWrite: true,
			Time: float64(t.Add(time.Duration(i)*time.Millisecond).UnixMilli()) / 1000})
	}
	if err := r.db.Enqueue(context.Background(), hbs, false); err != nil {
		r.t.Fatal(err)
	}
}

// serverRows returns the device's stored rows for minute m as
// "project credited" strings, and their total credit.
func (r *rig) serverRows(m time.Time) ([]string, int) {
	r.t.Helper()
	rows, err := r.st.Pool.Query(context.Background(),
		`SELECT project, credited FROM credited_minutes WHERE device_id = $1 AND minute = $2 ORDER BY project`, r.device, m)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	total := 0
	for rows.Next() {
		var p string
		var c int
		if err := rows.Scan(&p, &c); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %d", p, c))
		total += c
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out, total
}

// engineRows is the engine's latest output for minute m in the same form.
func (r *rig) engineRows(m time.Time) ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	total := 0
	for _, row := range r.computed[m] {
		out = append(out, fmt.Sprintf("%s %d", row.Project, row.Credited))
		total += row.Credited
	}
	slices.Sort(out)
	return out, total
}

// wantServerMatchesEngine checks that the server holds exactly the engine's
// rows for minute m.
func (r *rig) wantServerMatchesEngine(m time.Time) {
	r.t.Helper()
	got, gotTotal := r.serverRows(m)
	want, wantTotal := r.engineRows(m)
	if !slices.Equal(got, want) || gotTotal != wantTotal {
		r.t.Fatalf("server %s = %v (%d s), engine = %v (%d s)", m.Format("15:04"), got, gotTotal, want, wantTotal)
	}
	if len(got) > credited.MaxProjectsPerMinute {
		r.t.Fatalf("server %s has %d rows", m.Format("15:04"), len(got))
	}
}

var minute = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// Nine projects with equal signals in an open minute: the engine credits
// the first eight. A late signal moves the ninth into the top eight and the
// eighth out. The server must take the replacement in the same upload that
// drops the displaced project, so it holds what the engine computed (60 s),
// not 60 s less the displaced share with the ninth refused.
func TestOpenMinuteTopProjectsReplaced(t *testing.T) {
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
}

// The top eight of an open minute change several times within the rewrite
// window. After every run the server holds exactly the engine's rows, never
// more than eight; once final, the minute is frozen.
func TestOpenMinuteTopProjectsChurn(t *testing.T) {
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
}
