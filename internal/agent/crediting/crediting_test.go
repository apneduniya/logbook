package crediting

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/agent/upload"
	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/apneduniya/logbook/pkg/wire"
)

// env is a Runner wired to a real local database, the real device key and
// API client, and a fake server that follows the server contract. The
// engine is scripted.
type env struct {
	t   *testing.T
	dir string
	db  *localdb.DB
	srv *fakeServer
	url string

	mu      sync.Mutex
	now     time.Time
	apiKey  string
	script  map[time.Time][]struggle.Minute // minute → output
	inputs  []struggle.Input
	active  ActiveTimeSource
	runner  *Runner
	options func(*Options)
}

func newEnv(t *testing.T, start time.Time) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), now: start, apiKey: "key-1", script: map[time.Time][]struggle.Minute{}}
	e.srv = newFakeServer(e.clock)
	ts := httptest.NewServer(e.srv)
	t.Cleanup(ts.Close)
	e.url = ts.URL
	e.open()
	return e
}

// open (re)opens the database and builds a fresh Runner, as a daemon
// restart would.
func (e *env) open() {
	e.t.Helper()
	if e.db != nil {
		_ = e.db.Close()
	}
	db, err := localdb.Open(context.Background(), config.DBPath(e.dir), localdb.WithClock(e.clock))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = db.Close() })
	e.db = db
	client := serverapi.New(http.DefaultClient, "logbook-agent/test")
	o := Options{
		Store: db, Server: client,
		Identity: devicekey.NewIdentity(devicekey.Options{KeyPath: config.DeviceKeyPath(e.dir), Store: db,
			Registrar: client, Name: "test", OS: "linux", Now: e.clock}),
		Target: func() Target {
			e.mu.Lock()
			defer e.mu.Unlock()
			return Target{Target: serverapi.Target{BaseURL: e.url, APIKey: e.apiKey}}
		},
		ActiveTimes: e.active,
		Compute:     e.compute,
		TZ:          time.UTC,
		Now:         e.clock,
		Jitter:      func() float64 { return 0 },
		MinBackoff:  time.Second, MaxBackoff: time.Second,
	}
	if e.options != nil {
		e.options(&o)
	}
	e.runner = New(o)
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *env) setKey(k string) {
	e.mu.Lock()
	e.apiKey = k
	e.mu.Unlock()
}

// credit scripts the engine's output for one minute (Credited < 0 removes it).
func (e *env) credit(at time.Time, project string, secs int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if secs < 0 {
		delete(e.script, at)
		return
	}
	e.script[at] = []struggle.Minute{scripted(at, project, secs)}
}

// creditSplit scripts one minute credited to several projects.
func (e *env) creditSplit(at time.Time, secs map[string]int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ms []struggle.Minute
	for p, s := range secs {
		ms = append(ms, scripted(at, p, s))
	}
	e.script[at] = ms
}

func scripted(at time.Time, project string, secs int) struggle.Minute {
	label := struggle.LabelYou
	if secs == 0 {
		label = struggle.LabelIdle
	}
	return struggle.Minute{Minute: at, Project: project, Label: label, Credited: secs, Flags: []string{struggle.FlagLowSignal}}
}

func (e *env) compute(in struggle.Input) struggle.Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inputs = append(e.inputs, in)
	var out []struggle.Minute
	for at, ms := range e.script {
		if !at.Before(in.Window.From) && at.Before(in.Window.To) {
			out = append(out, ms...)
		}
	}
	// Plus an empty idle minute, which must never be uploaded.
	out = append(out, struggle.Minute{Minute: in.Window.From, Label: struggle.LabelIdle, Cuts: []string{struggle.CutIdle}})
	return struggle.Result{RulesVersion: struggle.RulesVersion, Minutes: out}
}

func (e *env) run() { e.runner.RunOnce(context.Background()) }

func (e *env) status() Status {
	e.t.Helper()
	s, err := LoadStatus(context.Background(), e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) deviceID() string {
	e.t.Helper()
	id, err := e.db.Get(context.Background(), devicekey.KeyID)
	if err != nil || id == "" {
		e.t.Fatalf("device id = %q, %v", id, err)
	}
	return id
}

func (e *env) wantServerMinute(at time.Time, project string, credited int) {
	e.t.Helper()
	m, ok := e.srv.minute(e.deviceID(), at, project)
	if !ok || m.Credited != credited {
		e.t.Fatalf("server minute %s %s = %+v (exists %v), want credited %d", at.Format("15:04"), project, m, ok, credited)
	}
}

var t0 = time.Date(2026, 11, 20, 12, 0, 5, 0, time.UTC)

func at(hhmm string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", "2026-11-20 "+hhmm)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMinutesUploadAndRewriteWindow(t *testing.T) {
	e := newEnv(t, t0)
	e.credit(at("11:58"), "app", 60)
	e.credit(at("11:59"), "app", 60)
	e.run()
	e.wantServerMinute(at("11:58"), "app", 60)
	e.wantServerMinute(at("11:59"), "app", 60)
	if b := e.srv.batches(); len(b) != 1 || len(b[0]) != 2 {
		t.Fatalf("batches = %+v, want one batch of 2 (the empty idle minute is never sent)", b)
	}
	if s := e.status(); s.Device.ID == "" || s.Minutes.LastSuccess.IsZero() || s.Minutes.Waiting != 0 || s.Minutes.LastComputed.IsZero() {
		t.Fatalf("status = %+v", s)
	}

	// Nothing changed: nothing is sent.
	e.advance(time.Minute)
	e.run()
	if n := len(e.srv.batches()); n != 1 {
		t.Fatalf("batches = %d, want 1 (unchanged minutes resent)", n)
	}

	// An open minute changes: only it is sent again.
	e.credit(at("11:59"), "app", 30)
	e.advance(time.Minute)
	e.run()
	b := e.srv.batches()
	if len(b) != 2 || len(b[1]) != 1 || b[1][0].Credited != 30 {
		t.Fatalf("second batch = %+v", b[len(b)-1])
	}
	e.wantServerMinute(at("11:59"), "app", 30)

	// An open minute moves to another project: the old row is sent first,
	// as a removal, so the server's 60-second cap and project limit hold.
	e.credit(at("11:58"), "web", 60)
	e.advance(time.Minute)
	e.run()
	b = e.srv.batches()
	last := b[len(b)-1]
	if len(last) != 2 || !isRemoval(last[0]) || last[0].Project != "app" || last[1].Project != "web" {
		t.Fatalf("project move batch = %+v", last)
	}
	if _, ok := e.srv.minute(e.deviceID(), at("11:58"), "app"); ok {
		t.Fatal("the server kept the removed project")
	}
	e.wantServerMinute(at("11:58"), "web", 60)

	// 40 minutes later 11:59 is final: a late change is not recomputed or sent.
	e.credit(at("11:59"), "app", 45)
	e.advance(37 * time.Minute)
	before := len(e.srv.batches())
	e.run()
	if n := len(e.srv.batches()); n != before {
		t.Fatalf("final minute was sent again: %+v", e.srv.batches()[before:])
	}
	e.wantServerMinute(at("11:59"), "app", 30)
}

func TestFinalMinuteSentOnceMore(t *testing.T) {
	e := newEnv(t, at("12:40").Add(5*time.Second))
	e.credit(at("12:15"), "app", 60)
	e.run()
	e.wantServerMinute(at("12:15"), "app", 60)

	// At 29 minutes old the minute changes while the server is down.
	e.advance(4 * time.Minute)
	e.credit(at("12:15"), "app", 50)
	e.srv.set(func(f *fakeServer) { f.status = http.StatusServiceUnavailable })
	e.run()
	if s := e.status(); s.Minutes.LastError == "" || s.Minutes.Waiting != 1 {
		t.Fatalf("status while down = %+v", s.Minutes)
	}
	// Back up once the minute is final: it is sent once more; this server
	// refuses a final rewrite, which is counted and not retried.
	e.srv.set(func(f *fakeServer) { f.status = 0 })
	e.advance(2 * time.Minute)
	e.run()
	e.wantServerMinute(at("12:15"), "app", 60)
	if s := e.status(); s.Minutes.Rejected != 1 || s.Minutes.Waiting != 0 || s.Minutes.LastError != "" {
		t.Fatalf("status = %+v", s.Minutes)
	}
	n := len(e.srv.batches())
	e.advance(time.Minute)
	e.run()
	if len(e.srv.batches()) != n {
		t.Fatal("a rejected final minute was retried")
	}
}

func TestOfflineCatchUpWritesFinalMinutes(t *testing.T) {
	e := newEnv(t, t0)
	e.srv.set(func(f *fakeServer) { f.status = http.StatusBadGateway })
	e.credit(at("11:50"), "app", 60)
	e.run()
	e.srv.set(func(f *fakeServer) { f.status = 0 })
	e.advance(45 * time.Minute) // 11:50 is now final, never sent
	e.run()
	e.wantServerMinute(at("11:50"), "app", 60)
}

func TestRejectionsAndBatchSplitting(t *testing.T) {
	e := newEnv(t, t0)
	e.srv.set(func(f *fakeServer) { f.rejectProject, f.maxBatch = "bad", 2 })
	for i := range 5 {
		e.credit(at("11:50").Add(time.Duration(i)*time.Minute), "app", 60)
	}
	e.credit(at("11:56"), "bad", 60)
	e.run()
	for i := range 5 {
		e.wantServerMinute(at("11:50").Add(time.Duration(i)*time.Minute), "app", 60)
	}
	s := e.status()
	if s.Minutes.Rejected != 1 || s.Minutes.Waiting != 0 {
		t.Fatalf("status = %+v", s.Minutes)
	}
	n := len(e.srv.batches())
	e.advance(time.Minute)
	e.run()
	if len(e.srv.batches()) != n {
		t.Fatal("a rejected minute was retried")
	}
}

func TestAuthPauseAndRevocation(t *testing.T) {
	e := newEnv(t, t0)
	e.credit(at("11:59"), "app", 60)
	e.setKey("wrong")
	e.run()
	if s := e.status(); s.Minutes.Paused != upload.ReasonReLogin || s.Device.ID != "" {
		t.Fatalf("status after 401 = %+v", s)
	}
	n := e.srv.requestCount()
	e.advance(time.Minute)
	e.run()
	if e.srv.requestCount() != n {
		t.Fatal("a paused runner sent requests with the same key")
	}
	// `logbook login` with the right key resumes at once.
	e.setKey("key-1")
	e.advance(time.Minute)
	e.run()
	e.wantServerMinute(at("11:59"), "app", 60)
	if s := e.status(); s.Minutes.Paused != "" {
		t.Fatalf("still paused: %+v", s.Minutes)
	}

	// The user revokes the device on the dashboard.
	id := e.deviceID()
	e.srv.set(func(f *fakeServer) { f.revoked[id] = true })
	e.credit(at("12:01"), "app", 60)
	e.advance(time.Minute)
	e.run()
	s := e.status()
	if s.Device.RevokedAt.IsZero() || s.Minutes.Paused == "" {
		t.Fatalf("status after revocation = %+v", s)
	}
	n = e.srv.requestCount()
	for range 3 {
		e.advance(time.Minute)
		e.run()
	}
	if e.srv.requestCount() != n {
		t.Fatal("a revoked device kept uploading or re-registered itself")
	}
}

func TestUnknownDeviceReRegisters(t *testing.T) {
	e := newEnv(t, t0)
	e.credit(at("11:59"), "app", 60)
	e.run()
	id := e.deviceID()
	// The server forgets the device (e.g. a fresh database).
	e.srv.set(func(f *fakeServer) {
		f.keys, f.byKey, f.minutes = map[string]ed25519.PublicKey{}, map[string]string{}, map[string]serverMinute{}
	})
	e.credit(at("12:00"), "app", 60)
	e.advance(time.Minute)
	e.run() // 404: forget and back off
	e.advance(2 * time.Second)
	e.run() // register again and upload
	if got := e.deviceID(); got == "" || got != id && len(got) != 36 {
		t.Fatalf("device id = %q", got)
	}
	e.wantServerMinute(at("12:00"), "app", 60)
}

func TestInputWindowAndOptionalSources(t *testing.T) {
	e := newEnv(t, t0)
	ctx := context.Background()
	if err := e.db.InsertEvents(ctx, []wire.LocalEvent{
		{Type: "activity", Activity: &wire.ActivitySample{Time: at("10:31"), FocusKind: "editor"}}, // before history
		{Type: "activity", Activity: &wire.ActivitySample{Time: at("10:30").Add(time.Second), FocusKind: "editor"}},
		{Type: "activity", Activity: &wire.ActivitySample{Time: at("11:59").Add(30 * time.Second), FocusKind: "editor"}},
		{Type: "activity", Activity: &wire.ActivitySample{Time: at("12:00"), FocusKind: "editor"}}, // current minute
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Enqueue(ctx, []wire.Heartbeat{{Entity: "/src/a.go", Time: float64(at("11:40").Unix()), Category: "ai coding"}}, false); err != nil {
		t.Fatal(err)
	}
	e.run()
	in := e.inputs[len(e.inputs)-1]
	if !in.Window.From.Equal(at("11:00")) || !in.Window.To.Equal(at("12:00")) || in.TZ != time.UTC {
		t.Fatalf("window = %+v", in.Window)
	}
	if len(in.Activity) != 3 || len(in.Heartbeats) != 1 || !in.Heartbeats[0].AI || in.ActiveTime != nil {
		t.Fatalf("input = %d activity, %+v heartbeats, %v active", len(in.Activity), in.Heartbeats, in.ActiveTime)
	}

	e.active = activeSource{{From: at("11:58"), To: at("11:59"), SessionID: "s", UserSecs: 40}}
	e.open()
	e.advance(time.Minute)
	e.run()
	if in := e.inputs[len(e.inputs)-1]; len(in.ActiveTime) != 1 {
		t.Fatalf("active time not passed: %+v", in.ActiveTime)
	}
}

type activeSource []struggle.ActiveTime

func (a activeSource) ActiveTimes(_ context.Context, from, to time.Time) ([]struggle.ActiveTime, error) {
	var out []struggle.ActiveTime
	for _, x := range a {
		if !x.To.Before(from) && x.From.Before(to) {
			out = append(out, x)
		}
	}
	return out, nil
}

func addDay(t *testing.T, db *localdb.DB, day string) {
	t.Helper()
	start, _, err := evidence.DayBounds(day)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.InsertEvents(ctx, []wire.LocalEvent{
		{Type: "activity", Activity: &wire.ActivitySample{Time: start.Add(9 * time.Hour), FocusKind: "editor", FocusApp: "Code"}},
		{Type: "ai", AI: &wire.AIEvent{Time: start.Add(9*time.Hour + time.Minute), Harness: wire.HarnessCodex, Kind: wire.AIPromptSubmit, SessionID: "s"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Enqueue(ctx, []wire.Heartbeat{{Entity: "/src/a.go", Time: float64(start.Add(10 * time.Hour).Unix()), Project: "app"}}, false); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceChainAcrossDays(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 17, 8, 0, 5, 0, time.UTC))
	e.run() // registers the device on 2026-11-17
	id := e.deviceID()
	addDay(t, e.db, "2026-11-16") // before registration: never sealed
	addDay(t, e.db, "2026-11-17")
	addDay(t, e.db, "2026-11-19") // 2026-11-18 has no events: a gap
	addDay(t, e.db, "2026-11-20")

	// 00:30 on the 21st: the 20th is still in its grace hour.
	e.mu.Lock()
	e.now = time.Date(2026, 11, 21, 0, 30, 0, 0, time.UTC)
	e.mu.Unlock()
	e.run()
	days := e.srv.storedDays(id)
	if len(days) != 2 || days[0].Day != "2026-11-17" || days[1].Day != "2026-11-19" {
		t.Fatalf("server days = %+v", days)
	}
	if days[0].PrevChainHash != "" || days[1].PrevChainHash != days[0].ChainHash {
		t.Fatal("chain not linked")
	}
	if days[0].EventCounts[evidence.TypeHeartbeat] != 1 || days[0].EventCounts[evidence.TypeAI] != 1 {
		t.Fatalf("event counts = %v", days[0].EventCounts)
	}
	// The server could recompute the content hash from a bundle of the same events.
	start, end, _ := evidence.DayBounds("2026-11-19")
	ev, _ := e.db.EventsBetween(context.Background(), start, end, nil)
	want, _ := evidence.ContentHash(evidence.DayEvents{Day: "2026-11-19", Activity: ev.Activity, Heartbeats: ev.Heartbeats, AI: ev.AI, Git: ev.Git})
	if days[1].ContentHash != want {
		t.Fatalf("content hash = %s, want %s", days[1].ContentHash, want)
	}

	// Restart after the grace hour: the chain resumes from local state.
	e.advance(time.Hour)
	e.open()
	addDay(t, e.db, "2026-11-19") // late events for a sealed day change nothing
	e.run()
	days = e.srv.storedDays(id)
	if len(days) != 3 || days[2].Day != "2026-11-20" || days[2].PrevChainHash != days[1].ChainHash {
		t.Fatalf("after restart, server days = %+v", days)
	}
	if s := e.status(); s.Evidence.LastDay != "2026-11-20" || s.Evidence.ChainHash != days[2].ChainHash || s.Evidence.State != "" {
		t.Fatalf("evidence status = %+v", s.Evidence)
	}

	// A day that fails transiently stays pending and is re-sent unchanged.
	addDay(t, e.db, "2026-11-21")
	e.srv.set(func(f *fakeServer) { f.dayStatus = http.StatusServiceUnavailable })
	e.advance(24 * time.Hour)
	e.run()
	pending, _, _ := e.db.LastEvidenceDay(context.Background(), id)
	if pending.Day != "2026-11-21" || pending.State != localdb.EvidencePending {
		t.Fatalf("last local day = %+v", pending)
	}
	addDay(t, e.db, "2026-11-21") // arrives after sealing: not part of the hash
	e.srv.set(func(f *fakeServer) { f.dayStatus = 0 })
	e.advance(5 * time.Second)
	e.run()
	days = e.srv.storedDays(id)
	if len(days) != 4 || days[3].ChainHash != pending.ChainHash {
		t.Fatalf("re-sent day = %+v, want the pending one", days[len(days)-1])
	}

	// A conflict stops the chain and shows in status.
	addDay(t, e.db, "2026-11-22")
	addDay(t, e.db, "2026-11-23")
	e.srv.set(func(f *fakeServer) { f.forceDayConflict = "2026-11-22" })
	e.advance(48 * time.Hour)
	e.run()
	s := e.status()
	if s.Evidence.State != localdb.EvidenceConflict || s.Evidence.Error == "" || s.Evidence.LastDay != "2026-11-21" {
		t.Fatalf("status after conflict = %+v", s.Evidence)
	}
	if days := e.srv.storedDays(id); len(days) != 4 {
		t.Fatalf("server days after conflict = %d", len(days))
	}
	n := e.srv.requestCount()
	e.advance(24 * time.Hour)
	e.run()
	if e.srv.requestCount() != n {
		t.Fatalf("requests after the conflict: %v", e.srv.requests[n:])
	}
	if last, _, _ := e.db.LastEvidenceDay(context.Background(), id); last.Day != "2026-11-22" {
		t.Fatalf("chain advanced past a conflict to %s", last.Day)
	}
}

func TestSpanArithmetic(t *testing.T) {
	tests := []struct {
		now                 string
		from, to, final     string
		lastSealable        string
		finalMinuteIsNotNow bool
	}{
		{"2026-11-20T12:00:05Z", "11:00", "12:00", "11:31", "2026-11-19", true},
		{"2026-11-20T12:30:00Z", "11:30", "12:30", "12:01", "2026-11-19", true},
		{"2026-11-21T00:59:59Z", "23:59", "00:59", "00:30", "2026-11-19", true},
		{"2026-11-21T01:00:00Z", "00:00", "01:00", "00:31", "2026-11-20", true},
	}
	for _, tt := range tests {
		now, _ := time.Parse(time.RFC3339, tt.now)
		s := spanAt(now)
		if s.from.Format("15:04") != tt.from || s.to.Format("15:04") != tt.to || s.finalBefore.Format("15:04") != tt.final {
			t.Errorf("spanAt(%s) = %s..%s final<%s", tt.now, s.from.Format("15:04"), s.to.Format("15:04"), s.finalBefore.Format("15:04"))
		}
		if !s.history.Equal(s.from.Add(-History)) {
			t.Errorf("history = %v", s.history)
		}
		if got := lastSealableDay(now); got != tt.lastSealable {
			t.Errorf("lastSealableDay(%s) = %s, want %s", tt.now, got, tt.lastSealable)
		}
	}
}

func TestMinuteRows(t *testing.T) {
	s := spanAt(t0)
	m := func(hhmm, project string, label struggle.Label, secs int, flags ...string) struggle.Minute {
		return struggle.Minute{Minute: at(hhmm), Project: project, Label: label, Credited: secs,
			Cuts: []string{"z", "a"}, Flags: flags}
	}
	rows := minuteRows(s, struggle.Result{RulesVersion: "v1", Minutes: []struggle.Minute{
		m("11:30", "b", struggle.LabelYou, 30),
		m("11:30", "a", struggle.LabelYou, 30),
		m("11:30", "a", struggle.LabelYou, 10),         // duplicate: dropped
		m("10:59", "a", struggle.LabelYou, 60),         // before the window
		m("12:00", "a", struggle.LabelYou, 60),         // the current minute
		m("11:31", "", struggle.LabelIdle, 0),          // says nothing: dropped
		m("11:32", "", struggle.LabelIdle, 0, "multi"), // flagged: kept
		m("11:33", "a", struggle.LabelAgent, 0),        // agent minute with a project: kept
	}})
	if len(rows) != 4 || rows[0].Project != "a" || rows[0].Credited != 30 || rows[1].Project != "b" ||
		rows[2].Project != "" || rows[3].Label != "agent" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Cuts[0] != "a" || rows[0].Digest == rows[1].Digest && rows[0].Credited != rows[1].Credited {
		t.Fatalf("cuts not sorted or digest wrong: %+v", rows[0])
	}
	z := superseded(rows[0])
	if z.Credited != 0 || z.Label != "idle" || len(z.Cuts) != 0 || z.Flags == nil || z.Digest == rows[0].Digest {
		t.Fatalf("superseded = %+v", z)
	}
}

func TestIsAIHeartbeat(t *testing.T) {
	n := 3
	tests := []struct {
		h    wire.Heartbeat
		want bool
	}{
		{wire.Heartbeat{Category: "coding"}, false},
		{wire.Heartbeat{Category: "AI Coding"}, true},
		{wire.Heartbeat{AIModel: "m"}, true},
		{wire.Heartbeat{AILineChanges: &n}, true},
		{wire.Heartbeat{HumanLineChanges: &n}, false},
	}
	for i, tt := range tests {
		if got := isAIHeartbeat(tt.h); got != tt.want {
			t.Errorf("case %d: isAIHeartbeat = %v", i, got)
		}
	}
}

func TestRunsNeverOverlap(t *testing.T) {
	e := newEnv(t, t0)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	e.options = func(o *Options) {
		o.Compute = func(struggle.Input) struggle.Result {
			entered <- struct{}{}
			<-release
			return struggle.Result{RulesVersion: struggle.RulesVersion}
		}
	}
	e.open()
	done := make(chan struct{})
	go func() { e.run(); close(done) }()
	<-entered
	e.run() // returns at once: the first run is still going
	close(release)
	<-done
	if len(entered) != 0 {
		t.Fatal("a second run started while the first was running")
	}
}

func TestNotSignedInComputesLocallyOnly(t *testing.T) {
	e := newEnv(t, t0)
	e.options = func(o *Options) {
		o.Target = func() Target { return Target{Disabled: "not signed in; run `logbook login`"} }
	}
	e.open()
	e.credit(at("11:59"), "app", 60)
	e.run()
	if e.srv.requestCount() != 0 {
		t.Fatal("requests sent while not signed in")
	}
	if s := e.status(); s.Minutes.Waiting != 1 || s.Minutes.Paused == "" {
		t.Fatalf("status = %+v", s.Minutes)
	}
}

func BenchmarkRunOnce(b *testing.B) {
	// Two weeks of history (8 busy hours a day) plus the last two hours of
	// a busy session on one device: a 5 s activity sample, a heartbeat every
	// 15 s, an AI event every 20 s (about 90 000 activity rows). Uploads are
	// off so the benchmark measures the per-minute local work. "warm" is
	// every run but the first after a start (the event cache holds the
	// window); "cold" is the first run, which reads the window from a
	// database holding weeks of rows.
	sixty := func(in struggle.Input) struggle.Result {
		out := make([]struggle.Minute, 0, 60)
		for m := in.Window.From; m.Before(in.Window.To); m = m.Add(time.Minute) {
			out = append(out, struggle.Minute{Minute: m, Project: "app", Label: struggle.LabelYou, Credited: 60})
		}
		return struggle.Result{RulesVersion: struggle.RulesVersion, Minutes: out}
	}
	for _, bc := range []struct {
		name    string
		compute func(struggle.Input) struggle.Result
		cold    bool
	}{
		{"engine/warm", struggle.Compute, false},
		{"engine/cold", struggle.Compute, true},
		{"sixty-minutes/warm", sixty, false},
		{"sixty-minutes/cold", sixty, true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			ctx := context.Background()
			now := t0
			db, err := localdb.Open(ctx, filepath.Join(b.TempDir(), "logbook.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			session := func(start time.Time, d time.Duration) {
				var evs []wire.LocalEvent
				var hbs []wire.Heartbeat
				for i := range int(d / (5 * time.Second)) {
					ts := start.Add(time.Duration(i) * 5 * time.Second)
					evs = append(evs, wire.LocalEvent{Type: "activity", Activity: &wire.ActivitySample{Time: ts, FocusKind: "editor", FocusApp: "Code"}})
					if i%3 == 0 {
						hbs = append(hbs, wire.Heartbeat{Entity: "/src/app/main.go", Time: float64(ts.UnixMicro()) / 1e6, Project: "app", IsWrite: i%2 == 0})
					}
					if i%4 == 0 {
						evs = append(evs, wire.LocalEvent{Type: "ai", AI: &wire.AIEvent{Time: ts, Harness: wire.HarnessClaudeCode, Kind: wire.AIToolStart, SessionID: "s", Cwd: "/src/app"}})
					}
				}
				if err := db.InsertEvents(ctx, evs); err != nil {
					b.Fatal(err)
				}
				if err := db.Enqueue(ctx, hbs, false); err != nil {
					b.Fatal(err)
				}
			}
			for day := 14; day >= 1; day-- {
				session(now.Add(-time.Duration(day)*24*time.Hour-10*time.Hour), 8*time.Hour)
			}
			session(now.Add(-2*time.Hour), 2*time.Hour)
			opts := Options{Store: db, Compute: bc.compute, Now: func() time.Time { return now },
				Target: func() Target { return Target{Disabled: "benchmark"} }}
			r := New(opts)
			b.ReportAllocs()
			for b.Loop() {
				if bc.cold {
					r = New(opts)
				}
				r.RunOnce(ctx)
			}
		})
	}
}

func TestEventCacheMatchesFullRead(t *testing.T) {
	e := newEnv(t, t0)
	ctx := context.Background()
	insert := func(ts time.Time, kind string) {
		t.Helper()
		if err := e.db.InsertEvents(ctx, []wire.LocalEvent{{Type: "activity", Activity: &wire.ActivitySample{Time: ts, FocusKind: kind}}}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.Enqueue(ctx, []wire.Heartbeat{{Entity: "/src/" + kind, Time: float64(ts.Unix()), Project: kind}}, false); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 90 {
		insert(e.clock().Add(-time.Duration(i)*time.Minute), "editor")
	}
	for step := range 40 {
		insert(e.clock().Add(-10*time.Second), "terminal")
		if step%7 == 0 {
			// Late arrivals: an editor's offline queue, the hook spool.
			insert(e.clock().Add(-50*time.Minute), "browser")
			insert(e.clock().Add(30*time.Second), "other") // in the current minute
		}
		e.run()
		in := e.inputs[len(e.inputs)-1]
		want, err := e.db.EventsBetween(ctx, in.Window.From.Add(-History), in.Window.To, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(in.Activity) != len(want.Activity) || len(in.Heartbeats) != len(want.Heartbeats) {
			t.Fatalf("step %d: cached %d/%d events, full read %d/%d", step, len(in.Activity), len(in.Heartbeats),
				len(want.Activity), len(want.Heartbeats))
		}
		for i := range want.Activity {
			if in.Activity[i] != want.Activity[i] {
				t.Fatalf("step %d: activity %d = %+v, want %+v", step, i, in.Activity[i], want.Activity[i])
			}
		}
		for i := range want.Heartbeats {
			if !in.Heartbeats[i].Time.Equal(want.Heartbeats[i].Timestamp()) {
				t.Fatalf("step %d: heartbeat %d differs", step, i)
			}
		}
		e.advance(time.Minute)
	}
}
