package upload

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/pkg/wire"
)

func items(n int) []localdb.Item {
	out := make([]localdb.Item, n)
	for i := range out {
		out[i] = localdb.Item{ID: int64(i + 1)}
	}
	return out
}

func TestClassify(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	perItem := `{"responses":[[{"data":{}},201],[{"error":"bad"},400],[{},409],[{},429],[{"error":"x"},422],[{},500]]}`
	tests := []struct {
		name                string
		status              int
		retryAfter, body    string
		n                   int
		want                outcome
		done, rejected, try int
		wantWait            time.Duration
	}{
		{"all ok", 201, "", `{"responses":[[{},201],[{},202]]}`, 2, outcomeOK, 2, 0, 0, 0},
		{"mixed items", 201, "", perItem, 6, outcomeOK, 2, 2, 2, 0},
		{"2xx without items", 202, "", `{}`, 3, outcomeOK, 3, 0, 0, 0},
		{"unauthorized", 401, "", `{"error":"unauthorized"}`, 2, outcomeAuth, 0, 0, 0, 0},
		{"forbidden", 403, "", ``, 2, outcomeAuth, 0, 0, 0, 0},
		{"rate limited", 429, "60", ``, 2, outcomeRetry, 0, 0, 0, time.Minute},
		{"server error", 503, "", ``, 2, outcomeRetry, 0, 0, 0, 0},
		{"bad batch", 400, "", `{"error":"Too many heartbeats"}`, 2, outcomeDrop, 0, 2, 0, 0},
		{"too large", 413, "", ``, 1, outcomeDrop, 0, 1, 0, 0},
		{"retry-after date", 503, now.Add(2 * time.Minute).Format(http.TimeFormat), ``, 1, outcomeRetry, 0, 0, 0, 2 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := classify(tt.status, tt.retryAfter, []byte(tt.body), items(tt.n), now)
			if r.outcome != tt.want || len(r.done) != tt.done || len(r.rejected) != tt.rejected || r.retry != tt.try {
				t.Fatalf("classify = outcome %d done %d rejected %d retry %d", r.outcome, len(r.done), len(r.rejected), r.retry)
			}
			if r.retryAfter != tt.wantWait {
				t.Fatalf("retryAfter = %v, want %v", r.retryAfter, tt.wantWait)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		failures int
		jitter   float64
		want     time.Duration
	}{
		{1, 0, 2500 * time.Millisecond},
		{1, 0.999999, 5 * time.Second},
		{2, 0, 5 * time.Second},
		{4, 0, 20 * time.Second},
		{20, 0, 5 * time.Minute},
		{1000, 0.5, 7*time.Minute + 30*time.Second},
	}
	for _, tt := range tests {
		got := Backoff(tt.failures, 5*time.Second, 10*time.Minute, tt.jitter)
		if got.Round(time.Millisecond) != tt.want {
			t.Errorf("Backoff(%d, %v) = %v, want %v", tt.failures, tt.jitter, got, tt.want)
		}
	}
}

func TestTargets(t *testing.T) {
	cfg := config.Config{ServerURL: "https://lb.example/", APIKey: "k",
		Hackatime: config.Hackatime{URL: "https://hackatime.hackclub.com/api/hackatime/v1/", APIKey: "h", Mirror: true}}
	if got := LogbookTarget(cfg).URL; got != "https://lb.example/api/v1/users/current/heartbeats.bulk" {
		t.Fatalf("logbook URL = %s", got)
	}
	if got := HackatimeTarget(cfg); got.URL != "https://hackatime.hackclub.com/api/hackatime/v1/users/current/heartbeats.bulk" || got.Disabled != "" {
		t.Fatalf("hackatime target = %+v", got)
	}
	cfg.Hackatime.Mirror = false
	cfg.APIKey = ""
	if HackatimeTarget(cfg).Disabled == "" || LogbookTarget(cfg).Disabled == "" {
		t.Fatal("expected disabled targets")
	}
}

// fakeServer answers bulk requests with a scripted handler and records them.
type fakeServer struct {
	mu      sync.Mutex
	handler func(w http.ResponseWriter, hbs []wire.Heartbeat)
	keys    []string
	batches [][]wire.Heartbeat
	headers []http.Header
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var hbs []wire.Heartbeat
	_ = json.NewDecoder(r.Body).Decode(&hbs)
	raw, _ := base64.StdEncoding.DecodeString(r.Header.Get("Authorization")[len("Basic "):])
	f.mu.Lock()
	f.keys = append(f.keys, string(raw))
	f.batches = append(f.batches, hbs)
	f.headers = append(f.headers, r.Header.Clone())
	h := f.handler
	f.mu.Unlock()
	h(w, hbs)
}

func acceptAll(w http.ResponseWriter, hbs []wire.Heartbeat) {
	resp := make([][2]any, len(hbs))
	for i := range hbs {
		resp[i] = [2]any{map[string]any{"data": map[string]any{}}, 201}
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"responses": resp})
}

type harness struct {
	db  *localdb.DB
	srv *fakeServer
	w   *Worker
	key atomic.Value
}

func newHarness(t *testing.T, batch int) *harness {
	t.Helper()
	db, err := localdb.Open(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &harness{db: db, srv: &fakeServer{handler: acceptAll}}
	ts := httptest.NewServer(h.srv)
	t.Cleanup(ts.Close)
	h.key.Store("key-1")
	h.w = New(Options{
		Name: "logbook", Queue: localdb.HeartbeatQueue, Store: db, Client: ts.Client(), UserAgent: "test-agent",
		Batch: batch, Jitter: func() float64 { return 0 },
		Target: func() Target {
			return Target{URL: ts.URL + "/bulk", APIKey: h.key.Load().(string), Headers: map[string]string{"X-Logbook-Source": "agent"}}
		},
	})
	return h
}

func (h *harness) enqueue(t *testing.T, n int) {
	t.Helper()
	hbs := make([]wire.Heartbeat, n)
	for i := range hbs {
		hbs[i] = wire.Heartbeat{Entity: fmt.Sprintf("/src/f%d.go", i), Time: 1795000000 + float64(i), Machine: "box"}
	}
	if err := h.db.Enqueue(context.Background(), hbs, false); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) pending(t *testing.T) int64 {
	t.Helper()
	n, err := h.db.Pending(context.Background(), localdb.HeartbeatQueue)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWorkerDrainsInBatches(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	h.enqueue(t, 25)
	if wait := h.w.cycle(ctx); wait != DefaultInterval {
		t.Fatalf("wait = %v", wait)
	}
	if n := h.pending(t); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
	if len(h.srv.batches) != 3 || len(h.srv.batches[0]) != 10 || len(h.srv.batches[2]) != 5 {
		t.Fatalf("batches = %d", len(h.srv.batches))
	}
	hdr := h.srv.headers[0]
	if hdr.Get("User-Agent") != "test-agent" || hdr.Get("X-Logbook-Source") != "agent" || hdr.Get("X-Machine-Name") != "box" {
		t.Fatalf("headers = %v", hdr)
	}
	if h.srv.keys[0] != "key-1" {
		t.Fatalf("key = %q", h.srv.keys[0])
	}
	st, _ := LoadState(ctx, h.db, "logbook")
	if st.LastSuccess.IsZero() || st.Paused != "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestWorkerPausesOnAuthAndResumesOnNewKey(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	h.enqueue(t, 3)
	h.srv.handler = func(w http.ResponseWriter, _ []wire.Heartbeat) { w.WriteHeader(http.StatusUnauthorized) }
	h.w.cycle(ctx)
	h.w.cycle(ctx) // still paused: no second request
	if len(h.srv.batches) != 1 {
		t.Fatalf("requests while paused = %d, want 1", len(h.srv.batches))
	}
	if st := h.w.State(); st.Paused != ReasonReLogin {
		t.Fatalf("live state = %+v", st)
	}
	if st, _ := LoadState(ctx, h.db, "logbook"); st.Paused != ReasonReLogin || st.LastError == "" {
		t.Fatalf("persisted state = %+v", st)
	}
	if n := h.pending(t); n != 3 {
		t.Fatalf("pending = %d, want 3 (nothing dropped on 401)", n)
	}

	h.srv.mu.Lock()
	h.srv.handler = acceptAll
	h.srv.mu.Unlock()
	h.key.Store("key-2")
	h.w.cycle(ctx)
	if n := h.pending(t); n != 0 {
		t.Fatalf("pending after re-login = %d", n)
	}
	if st := h.w.State(); st.Paused != "" {
		t.Fatalf("still paused: %+v", st)
	}
}

func TestWorkerBacksOffAndHonoursRetryAfter(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	h.enqueue(t, 2)
	h.srv.handler = func(w http.ResponseWriter, _ []wire.Heartbeat) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	if wait := h.w.cycle(ctx); wait != 2*time.Minute {
		t.Fatalf("wait after 429 = %v, want Retry-After 2m", wait)
	}
	h.srv.handler = func(w http.ResponseWriter, _ []wire.Heartbeat) { w.WriteHeader(http.StatusBadGateway) }
	w2 := h.w.cycle(ctx)
	w3 := h.w.cycle(ctx)
	if w2 != 5*time.Second || w3 != 10*time.Second {
		t.Fatalf("backoff = %v, %v; want 5s, 10s", w2, w3)
	}
	if n := h.pending(t); n != 2 {
		t.Fatalf("pending = %d, want 2", n)
	}
	if h.w.fullBatchWaiting(ctx) {
		t.Fatal("kick must not send while backing off")
	}
	h.srv.handler = acceptAll
	h.w.cycle(ctx)
	if n := h.pending(t); n != 0 {
		t.Fatalf("pending = %d after recovery", n)
	}
}

func TestWorkerDropsRejectedItems(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	h.enqueue(t, 3)
	h.srv.handler = func(w http.ResponseWriter, _ []wire.Heartbeat) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"responses":[[{},201],[{"error":"invalid"},400],[{},500]]}`))
	}
	h.w.cycle(ctx)
	if n := h.pending(t); n != 1 {
		t.Fatalf("pending = %d, want 1 (the 500 item)", n)
	}
	st, _ := LoadState(ctx, h.db, "logbook")
	if st.Rejected != 1 || st.LastError == "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestWorkerDisabledTarget(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 10)
	h.enqueue(t, 1)
	h.w.opt.Target = func() Target { return Target{Disabled: "not signed in"} }
	h.w.cycle(ctx)
	if len(h.srv.batches) != 0 || h.w.State().Paused != "not signed in" {
		t.Fatalf("disabled worker sent or did not report: %+v", h.w.State())
	}
}

func TestRunFlushesOnShutdown(t *testing.T) {
	h := newHarness(t, 10)
	h.w.opt.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.w.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond) // the first cycle runs on an empty queue
	h.enqueue(t, 2)
	cancel()
	<-done
	if n := h.pending(t); n != 0 {
		t.Fatalf("pending after shutdown flush = %d", n)
	}
}

func TestKickSendsFullBatch(t *testing.T) {
	h := newHarness(t, 5)
	h.w.opt.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.w.Run(ctx) }()
	time.Sleep(100 * time.Millisecond) // first cycle runs on an empty queue
	h.enqueue(t, 5)
	h.w.Kick()
	deadline := time.Now().Add(5 * time.Second)
	for h.pending(t) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("kick did not trigger an upload")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
