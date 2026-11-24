package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
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
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// rig is the agent's real crediting pipeline (local event log, real engine,
// HTTP client, device key) talking to the server's real minutes and
// evidence-day endpoints, services and Postgres store. The server's clock is
// the agent's plus serverOffset.
type rig struct {
	t      testing.TB
	st     *store.Store
	db     *localdb.DB
	dbPath string
	runner *crediting.Runner
	opts   crediting.Options
	client *serverapi.Client
	target serverapi.Target
	user   uuid.UUID
	device uuid.UUID
	key    *devicekey.Key
	// registered is the UTC day the device was registered.
	registered time.Time
	// identity is the device the agent runs as; takeover replaces it.
	identity *identity

	mu  sync.Mutex
	now time.Time
	// serverOffset is how far the server's clock is from the agent's.
	serverOffset time.Duration
	// date, between the agent and the server, rewrites every response's
	// Date header: "strip" removes it, "future" sets it a week ahead of the
	// server's clock, "" leaves it alone.
	date string
	// down makes the server unreachable (every request answers 503).
	down bool
	// computed is the engine's last output per minute, and input its last
	// input.
	computed map[time.Time][]struggle.Minute
	input    struggle.Input
	// repos is what the agent's repo source reports on each run.
	repos []struggle.Repo
	// beforeSnapshot runs once, at the start of the next run, before it
	// takes its snapshot of the log; afterSnapshot runs once, when the next
	// run calls the engine (after its snapshot, before it stores).
	beforeSnapshot, afterSnapshot func()

	// bundle holds the rows of each sealed block, read back from the log
	// and re-hashed as soon as the server accepts the block: what a step-5
	// bundle would carry.
	bundle map[string][]evidence.Row
	// pruned is set once a test deleted sealed rows from the local log:
	// minutes are then replayed only from the bundle.
	pruned bool
}

// config is how newRig sets up a rig.
type config struct {
	runner     []func(*crediting.Options)
	registered time.Time
	// prepare runs before the local database is opened, with the device
	// registered: a test can build an older database at r.dbPath.
	prepare func(r *rig)
}

type option func(*config)

// withSettle sets the settle period after a start or a wake.
func withSettle(d time.Duration) option {
	return withRunner(func(o *crediting.Options) { o.Settle = d })
}

// withRunner changes the Runner's options.
func withRunner(fn func(*crediting.Options)) option {
	return func(c *config) { c.runner = append(c.runner, fn) }
}

// registeredOn registers the device on day's UTC date instead of start's.
func registeredOn(day time.Time) option {
	return func(c *config) { c.registered = day.UTC().Truncate(24 * time.Hour) }
}

// beforeOpen runs fn before the local database is opened.
func beforeOpen(fn func(r *rig)) option { return func(c *config) { c.prepare = fn } }

// identity is the registered device the agent runs as. It changes only
// when a test replaces the device (rig.takeover).
type identity struct {
	mu  sync.Mutex
	dev devicekey.Device
}

func (i *identity) Ensure(context.Context, serverapi.Target) (devicekey.Device, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.dev, nil
}
func (*identity) MarkRevoked(context.Context, serverapi.Target) (bool, error) { return false, nil }
func (*identity) Confirm(context.Context, serverapi.Target) error             { return nil }
func (*identity) Forget(context.Context) error                                { return nil }

func (i *identity) set(dev devicekey.Device) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.dev = dev
}

// newRig starts a rig whose agent clock reads start, with no settle period
// after a start (Settle < 0) unless an option sets one.
func newRig(t testing.TB, start time.Time, opts ...option) *rig {
	t.Helper()
	c := config{registered: start.UTC().Truncate(24 * time.Hour)}
	for _, o := range opts {
		o(&c)
	}
	r := &rig{t: t, now: start, computed: map[time.Time][]struggle.Minute{}, bundle: map[string][]evidence.Row{},
		registered: c.registered}
	r.st = storetest.New(t)
	r.user = seedtest.User(t, r.st, "alice", "UTC", true)
	var err error
	r.key, err = devicekey.Create(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	r.device, err = r.st.SeedTestDevice(context.Background(), db.SeedTestDeviceParams{
		UserID: r.user, Name: "laptop", PublicKey: r.key.Public(), CreatedAt: r.registered,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The endpoints' rate limiters run on the real clock, which barely
	// moves while the rig's clock runs through hours: each agent run (each
	// reading of the rig's clock) gets fresh handlers, so a simulated
	// minute has the budget a real one has.
	var (
		mux   *http.ServeMux
		muxAt time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		date, down, server := r.date, r.down, r.now.Add(r.serverOffset)
		if mux == nil || !muxAt.Equal(r.now) {
			mux, muxAt = http.NewServeMux(), r.now
			credited.NewHandler(credited.NewService(r.st, nil, credited.WithClock(r.serverClock)), httpapitest.StubSession).Register(mux)
			ledger.NewHandler(ledger.NewService(r.st, ledger.WithClock(r.serverClock)), httpapitest.StubSession).Register(mux)
		}
		handler := mux
		r.mu.Unlock()
		switch date {
		case "strip":
			w.Header()["Date"] = nil // net/http then sends none
		case "future":
			w.Header().Set("Date", server.Add(7*24*time.Hour).UTC().Format(http.TimeFormat))
		}
		if down {
			http.Error(w, `{"error": "unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		req.Header.Set(httpapitest.UserHeader, r.user.String())
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	r.client = serverapi.New(srv.Client(), "logbook-agent/e2e")
	r.target = serverapi.Target{BaseURL: srv.URL, APIKey: "key"}

	r.dbPath = filepath.Join(t.TempDir(), "agent.db")
	if c.prepare != nil {
		c.prepare(r)
	}
	r.db = r.openDB(r.dbPath)
	r.identity = &identity{dev: devicekey.Device{ID: r.device.String(), RegisteredAt: r.registered, Key: r.key}}
	r.opts = crediting.Options{
		Store: r.db, Server: r.client,
		Identity: r.identity,
		Target:   func() crediting.Target { return crediting.Target{Target: r.target} },
		Repos:    r, Compute: r.compute, TZ: time.UTC, Now: r.clock,
		Jitter: func() float64 { return 0 },
		Settle: -1,
		Salts:  zeroSalts{},
	}
	for _, fn := range c.runner {
		fn(&r.opts)
	}
	r.runner = crediting.New(r.opts)
	return r
}

// takeover registers a new device for the same user, with a new key, on
// the UTC day of the agent's clock, and makes the agent run as it from the
// next run on, as when the previous device's key was replaced (ADR 0017
// point 5). The rig's checks then look at the new device.
func (r *rig) takeover() {
	r.t.Helper()
	key, err := devicekey.Create(filepath.Join(r.t.TempDir(), "device-2.key"))
	if err != nil {
		r.t.Fatal(err)
	}
	day := r.clock().UTC().Truncate(24 * time.Hour)
	id, err := r.st.SeedTestDevice(context.Background(), db.SeedTestDeviceParams{
		UserID: r.user, Name: "new laptop", PublicKey: key.Public(), CreatedAt: day,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.device, r.key, r.registered = id, key, day
	r.identity.set(devicekey.Device{ID: id.String(), RegisteredAt: day, Key: key})
}

// openDB opens a local database at path on the agent's clock.
func (r *rig) openDB(path string) *localdb.DB {
	r.t.Helper()
	d, err := localdb.Open(context.Background(), path, localdb.WithClock(r.clock))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = d.Close() })
	return d
}

// restart replaces the Runner with a new one, as a daemon restart does.
func (r *rig) restart(opts ...func(*crediting.Options)) {
	o := r.opts
	for _, fn := range opts {
		fn(&o)
	}
	r.runner = crediting.New(o)
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

// serverClock is the server's clock.
func (r *rig) serverClock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now.Add(r.serverOffset)
}

// with changes the rig's settings under its lock.
func (r *rig) with(fn func(r *rig)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r)
}

func (r *rig) set(now time.Time) {
	r.mu.Lock()
	r.now = now
	r.mu.Unlock()
}

// ActiveRepos is the agent's repo source: it runs beforeSnapshot (the run
// calls it before taking its snapshot) and reports r.repos.
func (r *rig) ActiveRepos(context.Context) ([]struggle.Repo, error) {
	r.mu.Lock()
	hook, repos := r.beforeSnapshot, slices.Clone(r.repos)
	r.beforeSnapshot = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return repos, nil
}

// compute is the real engine, remembering its input and output and running
// afterSnapshot first.
func (r *rig) compute(in struggle.Input) struggle.Result {
	r.mu.Lock()
	hook := r.afterSnapshot
	r.afterSnapshot = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	res := struggle.Compute(in)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.input = in
	byMinute := map[time.Time][]struggle.Minute{}
	for _, m := range res.Minutes {
		byMinute[m.Minute] = append(byMinute[m.Minute], m)
	}
	for m, rows := range byMinute {
		r.computed[m] = rows
	}
	return res
}

// run runs the pipeline once and fails the test if the server rejected a
// minute (other than as final, with its rows) or an upload failed.
func (r *rig) run() {
	r.t.Helper()
	r.runAny()
	s := r.status()
	if s.Minutes.Rejected != 0 || s.Minutes.LastError != "" || s.Minutes.Refused != "" || s.Evidence.Error != "" ||
		s.Evidence.LocalError != "" {
		r.t.Fatalf("after run at %s: status %+v", r.clock().Format(time.DateTime), s)
	}
}

// runAny runs the pipeline once without checking its status.
func (r *rig) runAny() {
	r.t.Helper()
	r.runner.RunOnce(context.Background())
	r.captureBlocks()
}

// runEvery runs the agent once a minute, 5 s past it, for minutes [from, to),
// calling before (if not nil) ahead of each run.
func (r *rig) runEvery(from, to time.Time, before func(now time.Time)) {
	r.t.Helper()
	for at := from; at.Before(to); at = at.Add(time.Minute) {
		now := at.Add(5 * time.Second)
		r.set(now)
		if before != nil {
			before(now)
		}
		r.run()
	}
}

func (r *rig) status() crediting.Status {
	r.t.Helper()
	s, err := crediting.LoadStatus(context.Background(), r.db)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// high is the local log's high-water mark.
func (r *rig) high() int64 {
	r.t.Helper()
	w, err := r.db.ReadWindow(context.Background(), time.Time{}, time.Time{}, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return w.H
}

// Events. Each is logged as the loopback API, the spool drainer or the
// sensors log it, taking the next sequence numbers.

// store logs raw local events.
func (r *rig) store(evs ...wire.LocalEvent) {
	r.t.Helper()
	if err := r.db.InsertEvents(context.Background(), evs); err != nil {
		r.t.Fatal(err)
	}
}

// present records the person at the editor from from to to: an activity
// sample every 10 seconds with fresh input.
func (r *rig) present(from, to time.Time) {
	r.t.Helper()
	var evs []wire.LocalEvent
	for t := from; t.Before(to); t = t.Add(10 * time.Second) {
		evs = append(evs, sample(t, "editor"))
	}
	r.store(evs...)
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
	r.enqueue(hbs...)
}

// heartbeat logs one editor heartbeat for app's main.go at line 10: a save
// when write, otherwise the person reading.
func (r *rig) heartbeat(t time.Time, write bool) {
	r.t.Helper()
	r.enqueue(appHeartbeat(t, write))
}

func appHeartbeat(t time.Time, write bool) wire.Heartbeat {
	return wire.Heartbeat{Entity: "/src/app/main.go", Project: "app", IsWrite: write, Lineno: new(10), Cursorpos: new(1),
		Time: float64(t.UnixMilli()) / 1000}
}

func (r *rig) enqueue(hbs ...wire.Heartbeat) {
	r.t.Helper()
	if err := r.db.Enqueue(context.Background(), hbs, false); err != nil {
		r.t.Fatal(err)
	}
}

func sample(t time.Time, focus string) wire.LocalEvent {
	return wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{Time: t, IdleSeconds: 1, FocusKind: focus}}
}

func ai(t time.Time, kind wire.AIEventKind) wire.LocalEvent {
	e := wire.AIEvent{Time: t, Harness: wire.HarnessClaudeCode, Kind: kind, SessionID: "cc", Project: "app", Cwd: "/src/app"}
	if kind == wire.AIPromptSubmit {
		e.PromptChars, e.PromptHash = 60, strings.Repeat("ab", 32) // the same long prompt every time
	}
	return wire.LocalEvent{Type: "ai", AI: &e}
}

// Rows as compared.

var minute = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// m is minute + i minutes.
func m(i int) time.Time { return minute.Add(time.Duration(i) * time.Minute) }

// isRemoval reports whether a stored row is a removal sent to the server
// (idle, no credit, cuts or flags): the engine never emits it.
func isRemoval(c localdb.CreditedMinute) bool {
	return c.Label == string(struggle.LabelIdle) && c.Credited == 0 && len(c.Cuts) == 0 && len(c.Flags) == 0
}

// mustStored is the agent's stored rows for minute at.
func (r *rig) mustStored(at time.Time) []localdb.CreditedMinute {
	r.t.Helper()
	stored, err := r.db.StoredMinutes(context.Background(), at, at.Add(time.Minute))
	if err != nil {
		r.t.Fatal(err)
	}
	return slices.DeleteFunc(stored, isRemoval)
}

// storedCredit is the agent's stored credit for minute at.
func (r *rig) storedCredit(at time.Time) int {
	r.t.Helper()
	total := 0
	for _, s := range r.mustStored(at) {
		total += s.Credited
	}
	return total
}

// keyOf is the key of the agent's rows for minute at (zero if none).
func (r *rig) keyOf(at time.Time) localdb.Key {
	r.t.Helper()
	rows := r.mustStored(at)
	if len(rows) == 0 {
		return localdb.Key{}
	}
	return rows[0].Key
}

// serverRows returns the device's stored rows for minute at as "project
// credited" strings, and their total credit.
func (r *rig) serverRows(at time.Time) ([]string, int) {
	r.t.Helper()
	var out []string
	total := 0
	for _, row := range r.serverMinutes(at, at.Add(time.Minute)) {
		out = append(out, fmt.Sprintf("%s %d", row.Project, row.Credited))
		total += row.Credited
	}
	return out, total
}

// engineRows is the engine's latest output for minute at in the same form.
func (r *rig) engineRows(at time.Time) ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	total := 0
	for _, row := range r.computed[at] {
		out = append(out, fmt.Sprintf("%s %d", row.Project, row.Credited))
		total += row.Credited
	}
	slices.Sort(out)
	return out, total
}

// wantServerMatchesEngine checks that the server holds exactly the engine's
// latest rows for minute at.
func (r *rig) wantServerMatchesEngine(at time.Time) {
	r.t.Helper()
	got, gotTotal := r.serverRows(at)
	want, wantTotal := r.engineRows(at)
	if !slices.Equal(got, want) || gotTotal != wantTotal {
		r.t.Fatalf("server %s = %v (%d s), engine = %v (%d s)", at.Format("15:04"), got, gotTotal, want, wantTotal)
	}
	if len(got) > credited.MaxProjectsPerMinute {
		r.t.Fatalf("server %s has %d rows", at.Format("15:04"), len(got))
	}
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
