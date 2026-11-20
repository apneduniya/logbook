package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/pkg/wire"
)

const (
	logbookKey   = "waka_00000000-0000-4000-8000-00000000000a"
	hackatimeKey = "00000000-0000-4000-8000-00000000000b"
)

// fakeUpstream is a fake Logbook or Hackatime bulk endpoint.
type fakeUpstream struct {
	path, key string
	down      atomic.Bool

	mu   sync.Mutex
	got  []wire.Heartbeat
	errs []string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != f.path {
		f.errs = append(f.errs, "unexpected "+r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(f.key)) {
		f.errs = append(f.errs, "wrong key")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(r.UserAgent(), "logbook-agent/") {
		f.errs = append(f.errs, "user agent "+r.UserAgent())
	}
	var hbs []wire.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hbs); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.got = append(f.got, hbs...)
	resp := make([][2]any, len(hbs))
	for i := range hbs {
		resp[i] = [2]any{map[string]any{"data": map[string]any{}}, http.StatusCreated}
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"responses": resp})
}

func (f *fakeUpstream) received() ([]wire.Heartbeat, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wire.Heartbeat(nil), f.got...), append([]string(nil), f.errs...)
}

type env struct {
	dir                string
	cfg                config.Config
	logbook, hackatime *fakeUpstream
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		dir:       t.TempDir(),
		logbook:   &fakeUpstream{path: "/api/v1/users/current/heartbeats.bulk", key: logbookKey},
		hackatime: &fakeUpstream{path: "/api/hackatime/v1/users/current/heartbeats.bulk", key: hackatimeKey},
	}
	lb := httptest.NewServer(e.logbook)
	ht := httptest.NewServer(e.hackatime)
	t.Cleanup(lb.Close)
	t.Cleanup(ht.Close)
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL, cfg.APIKey, cfg.LocalAddr = lb.URL, logbookKey, freeAddr(t)
	cfg.Hackatime = config.Hackatime{URL: ht.URL + "/api/hackatime/v1", APIKey: hackatimeKey, Mirror: true}
	if err := config.Save(e.dir, cfg); err != nil {
		t.Fatal(err)
	}
	e.cfg = cfg
	return e
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// start runs the daemon and returns a stop function that waits for it.
func (e *env) start(t *testing.T, mutate ...func(*Options)) (stop func() error) {
	t.Helper()
	o := Options{
		DataDir: e.dir, Config: e.cfg, Version: "test", UserAgent: "logbook-agent/test (linux-amd64)",
		uploadInterval: 50 * time.Millisecond, restartFloor: 10 * time.Millisecond, spoolGrace: time.Millisecond,
		settingsEvery: 10 * time.Millisecond,
	}
	for _, m := range mutate {
		m(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, o) }()
	waitFor(t, "daemon to listen", func() bool { return Probe(context.Background(), e.cfg.LocalAddr) })
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() { cancel(); err = <-errc })
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// postLikeWakatimeCLI sends what wakatime-cli sends after the installer
// points api_url at the agent.
func (e *env) postLikeWakatimeCLI(t *testing.T, n int) {
	t.Helper()
	hbs := make([]map[string]any, n)
	for i := range hbs {
		hbs[i] = map[string]any{
			"entity": "/home/me/app/main.go", "type": "file", "category": "coding",
			"time":    float64(time.Now().Add(-time.Duration(n-i)*time.Second).UnixMicro()) / 1e6,
			"project": "app", "language": "Go", "is_write": i%2 == 0, "lineno": i + 1,
		}
	}
	body, _ := json.Marshal(hbs)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+e.cfg.LocalAddr+"/api/v1/users/current/heartbeats.bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(logbookKey)))
	req.Header.Set("User-Agent", "wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 vscode/1.95.3 vscode-wakatime/24.9.1")
	req.Header.Set("X-Machine-Name", "laptop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("local bulk = %d", resp.StatusCode)
	}
}

func (e *env) localStatus(t *testing.T) status.Snapshot {
	t.Helper()
	s, err := status.Fetch(t.Context(), http.DefaultClient, e.cfg.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEndToEndUploadAndMirror(t *testing.T) {
	e := newEnv(t)
	stop := e.start(t)
	e.postLikeWakatimeCLI(t, 3)

	for _, up := range []*fakeUpstream{e.logbook, e.hackatime} {
		waitFor(t, up.path, func() bool { got, _ := up.received(); return len(got) == 3 })
		got, errs := up.received()
		if len(errs) > 0 {
			t.Fatalf("%s errors: %v", up.path, errs)
		}
		if got[0].Machine != "laptop" || !strings.HasPrefix(got[0].UserAgent, "wakatime/") || *got[2].Lineno != 3 {
			t.Fatalf("%s got %+v", up.path, got[0])
		}
	}
	waitFor(t, "queues to empty", func() bool {
		s := e.localStatus(t)
		return s.Logbook.Queue.Pending == 0 && s.Hackatime.Queue.Pending == 0
	})
	s := e.localStatus(t)
	if !s.DaemonRunning || s.Logbook.Queue.Pending != 0 || s.Hackatime.Queue.Pending != 0 || s.Logbook.Worker.LastSuccess.IsZero() {
		t.Fatalf("status = %+v", s)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestRestartResumesQueues(t *testing.T) {
	e := newEnv(t)
	e.logbook.down.Store(true)
	e.hackatime.down.Store(true)
	stop := e.start(t)
	e.postLikeWakatimeCLI(t, 2)
	waitFor(t, "a failed attempt", func() bool { return e.localStatus(t).Logbook.Worker.LastError != "" })
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	db := openDB(t, e.dir)
	for _, q := range []localdb.Queue{localdb.HeartbeatQueue, localdb.MirrorQueue} {
		if n, _ := db.Pending(context.Background(), q); n != 2 {
			t.Fatalf("%s = %d after stop, want 2", q, n)
		}
	}
	_ = db.Close()

	e.logbook.down.Store(false)
	e.hackatime.down.Store(false)
	e.start(t)
	for _, up := range []*fakeUpstream{e.logbook, e.hackatime} {
		waitFor(t, "resumed upload to "+up.path, func() bool { got, _ := up.received(); return len(got) == 2 })
	}
}

func TestSingleInstance(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	err := Run(context.Background(), Options{DataDir: e.dir, Config: e.cfg})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run = %v, want ErrAlreadyRunning", err)
	}
}

func TestLoopbackAddr(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"127.0.0.1:47231", "127.0.0.1:47231", true},
		{"localhost:47231", "127.0.0.1:47231", true},
		{"0.0.0.0:47231", "", false},
		{"192.168.1.2:47231", "", false},
		{"[::1]:47231", "", false},
		{"127.0.0.1", "", false},
		{"127.0.0.1:0", "", false},
	}
	for _, tt := range tests {
		got, err := LoopbackAddr(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("LoopbackAddr(%q) = %q, %v", tt.in, got, err)
		}
	}
}

// flakySensor panics on its first runs, then records samples until stopped.
type flakySensor struct{ runs atomic.Int32 }

func (s *flakySensor) Name() string { return "flaky" }

func (s *flakySensor) Run(ctx context.Context, sink Sink) error {
	switch s.runs.Add(1) {
	case 1:
		panic("sensor bug")
	case 2:
		return errors.New("sensor gave up")
	}
	if err := sink.RecordActivity(ctx, wire.ActivitySample{Time: time.Now().UTC(), FocusKind: "editor"}); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// repoSensor records which repos the daemon reports.
type repoSensor struct {
	repos RepoSource
	seen  atomic.Int32
}

func (s *repoSensor) Name() string { return "repos" }

func (s *repoSensor) Run(ctx context.Context, _ Sink) error {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if _, err := s.repos.ActiveRepos(ctx); err == nil {
				s.seen.Add(1)
			}
		}
	}
}

func TestDaemonSurvivesPanickingSensor(t *testing.T) {
	e := newEnv(t)
	flaky := &flakySensor{}
	rs := &repoSensor{}
	e.start(t, func(o *Options) {
		o.Sensors = []Sensor{flaky}
		o.RepoSensors = []func(RepoSource) Sensor{func(r RepoSource) Sensor { rs.repos = r; return rs }}
	})
	waitFor(t, "sensor restart", func() bool { return e.localStatus(t).Today.ActivitySamples == 1 })
	if flaky.runs.Load() != 3 {
		t.Fatalf("runs = %d, want 3", flaky.runs.Load())
	}
	waitFor(t, "repo sensor", func() bool { return rs.seen.Load() > 0 })
	e.postLikeWakatimeCLI(t, 1) // the daemon still serves
}

func TestDaemonDrainsSpoolOnStart(t *testing.T) {
	e := newEnv(t)
	for i := range 3 {
		appendSpool(t, config.SpoolPath(e.dir), i)
	}
	e.start(t)
	waitFor(t, "spool drain", func() bool { return e.localStatus(t).Today.AIEvents == 3 })
}

func TestDaemonPicksUpNewLogin(t *testing.T) {
	e := newEnv(t)
	e.cfg.APIKey = ""
	if err := config.Save(e.dir, e.cfg); err != nil {
		t.Fatal(err)
	}
	e.start(t)
	waitFor(t, "signed-out status", func() bool { return e.localStatus(t).Logbook.Worker.Paused != "" })

	// Make the file's mtime differ even on coarse-timestamp filesystems.
	time.Sleep(20 * time.Millisecond)
	e.cfg.APIKey = logbookKey
	if err := config.Save(e.dir, e.cfg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reloaded config", func() bool {
		s := e.localStatus(t)
		return s.SignedIn && s.Logbook.Worker.Paused == ""
	})
}
