package localapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/internal/agent/statuspage"
	"github.com/apneduniya/logbook/pkg/wire"
)

const (
	testKey   = "waka_00000000-0000-4000-8000-000000000001"
	testToken = "0123456789abcdef0123456789abcdef0123456789abcdef"
	testPort  = 47231

	testHackatimeKey = "hackatime-secret-key-0001"
)

type fixture struct {
	db      *localdb.DB
	srv     *Server
	h       http.Handler
	cfg     config.Config
	kicks   atomic.Int32
	nowFunc func() time.Time
}

func newFixture(t *testing.T, serverURL string) *fixture {
	t.Helper()
	db, err := localdb.Open(context.Background(), filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	page, err := statuspage.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, nowFunc: time.Now, cfg: config.Config{
		ServerURL: serverURL, APIKey: testKey, LocalToken: testToken, LocalAddr: "127.0.0.1:47231",
		Hackatime: config.Hackatime{URL: "https://hackatime.example/api/hackatime/v1", APIKey: testHackatimeKey, Mirror: true},
	}}
	f.srv = New(Options{
		Settings: func() config.Config { return f.cfg },
		Store:    db,
		Status: func(ctx context.Context) (status.Snapshot, error) {
			return status.Collect(ctx, status.Input{Store: db, Config: f.cfg, Now: time.Now()})
		},
		Page: page, Client: &http.Client{Timeout: 2 * time.Second}, UserAgent: "logbook-agent/test",
		OnEnqueue:     func() { f.kicks.Add(1) },
		MirrorEnabled: func(c config.Config) bool { return c.Hackatime.Mirror },
		Now:           func() time.Time { return f.nowFunc() },
	})
	f.h = f.srv.Handler(testPort)
	return f
}

// do sends a request as if from wakatime-cli on this machine.
func (f *fixture) do(t *testing.T, method, path string, body any, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rd)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Host = "127.0.0.1:47231"
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func basicKey(key string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(key)))
	}
}

func wakatimeCLI(r *http.Request) {
	basicKey(testKey)(r)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 vscode/1.95.3 vscode-wakatime/24.9.1")
	r.Header.Set("X-Machine-Name", "my%20laptop")
}

func beat(entity string) map[string]any {
	return map[string]any{
		"entity": entity, "type": "file", "category": "coding", "time": float64(time.Now().Unix()) - 5,
		"project": "app", "language": "Go", "is_write": true, "lineno": 3, "cursorpos": 10, "lines": 40,
	}
}

func TestGuard(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1")
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"loopback ipv4", func(*http.Request) {}, http.StatusOK},
		{"localhost host", func(r *http.Request) { r.Host = "localhost:47231" }, http.StatusOK},
		{"loopback ipv6", func(r *http.Request) { r.RemoteAddr = "[::1]:5000" }, http.StatusOK},
		{"remote addr", func(r *http.Request) { r.RemoteAddr = "192.168.1.20:5000" }, http.StatusForbidden},
		{"rebinding host", func(r *http.Request) { r.Host = "evil.example:47231" }, http.StatusForbidden},
		{"wrong port", func(r *http.Request) { r.Host = "127.0.0.1:8080" }, http.StatusForbidden},
		{"no port", func(r *http.Request) { r.Host = "localhost" }, http.StatusForbidden},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		{"same origin", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:47231") }, http.StatusOK},
		{"cross-site fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, "/local/v1/status", nil, tt.mutate)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestBulkQueuesAndAnswersLikeWakaTime(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1")
	bad := beat("")
	rec := f.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk",
		[]any{beat("/src/app/main.go"), bad, beat("/src/app/util.go")}, wakatimeCLI)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Responses [][2]json.RawMessage `json:"responses"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Responses) != 3 {
		t.Fatalf("body = %s", rec.Body)
	}
	codes := []string{string(resp.Responses[0][1]), string(resp.Responses[1][1]), string(resp.Responses[2][1])}
	if strings.Join(codes, ",") != "201,400,201" {
		t.Fatalf("codes = %v", codes)
	}
	var data struct {
		Data heartbeatData `json:"data"`
	}
	if err := json.Unmarshal(resp.Responses[0][0], &data); err != nil || len(data.Data.ID) != 64 || data.Data.Machine != "my laptop" {
		t.Fatalf("item = %s", resp.Responses[0][0])
	}
	ctx := context.Background()
	for _, q := range []localdb.Queue{localdb.HeartbeatQueue, localdb.MirrorQueue} {
		items, err := f.db.Peek(ctx, q, 10)
		if err != nil || len(items) != 2 {
			t.Fatalf("%s = %d items, %v", q, len(items), err)
		}
		hb := items[0].Heartbeat
		if hb.Machine != "my laptop" || !strings.HasPrefix(hb.UserAgent, "wakatime/v1.102.1") || hb.Lineno == nil || *hb.Lineno != 3 {
			t.Fatalf("queued heartbeat = %+v", hb)
		}
	}
	if f.kicks.Load() != 1 {
		t.Fatalf("kicks = %d", f.kicks.Load())
	}
}

func TestSingleHeartbeat(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1")
	f.cfg.Hackatime.Mirror = false
	tests := []struct {
		path string
		body any
		want int
	}{
		{"/api/v1/users/current/heartbeats", beat("/a.go"), http.StatusCreated},
		{"/api/hackatime/v1/users/current/heartbeats", []any{beat("/b.go")}, http.StatusAccepted},
		{"/api/v1/users/current/heartbeats", []any{beat("/a.go"), beat("/b.go")}, http.StatusBadRequest},
		{"/api/v1/users/current/heartbeats", beat(""), http.StatusBadRequest},
		{"/api/v1/users/current/heartbeats", "not json", http.StatusBadRequest},
		{"/api/v1/users/current/heartbeats", "[]", http.StatusBadRequest},
		{"/api/hackatime/v1/users/current/heartbeats.bulk", make([]any, 101), http.StatusBadRequest},
	}
	for _, tt := range tests {
		rec := f.do(t, http.MethodPost, tt.path, tt.body, wakatimeCLI)
		if rec.Code != tt.want {
			t.Errorf("%s %v: status = %d, want %d (%s)", tt.path, tt.body, rec.Code, tt.want, rec.Body)
		}
	}
	if n, _ := f.db.Pending(context.Background(), localdb.MirrorQueue); n != 0 {
		t.Fatalf("mirror queue = %d with mirroring off", n)
	}
	if n, _ := f.db.Pending(context.Background(), localdb.HeartbeatQueue); n != 2 {
		t.Fatalf("heartbeat queue = %d, want 2", n)
	}
}

func TestHeartbeatAuth(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1")
	enc := base64.StdEncoding.EncodeToString
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"basic", basicKey(testKey), http.StatusCreated},
		{"basic with colon", func(r *http.Request) { r.Header.Set("Authorization", "Basic "+enc([]byte(testKey+":"))) }, http.StatusCreated},
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testKey) }, http.StatusCreated},
		{"query", func(r *http.Request) { r.URL.RawQuery = "api_key=" + testKey }, http.StatusCreated},
		{"missing", func(*http.Request) {}, http.StatusUnauthorized},
		{"wrong key", basicKey("waka_nope"), http.StatusUnauthorized},
		{"garbage", func(r *http.Request) { r.Header.Set("Authorization", "Basic !!!") }, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", []any{beat("/a.go")}, tt.mutate)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
	f.cfg.APIKey = ""
	if rec := f.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", []any{beat("/a.go")},
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }); rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty key accepted when signed out: %d", rec.Code)
	}
}

func TestLocalEvents(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1")
	now := time.Now().UTC()
	good := []wire.LocalEvent{
		{Type: "ai", AI: &wire.AIEvent{Time: now, Harness: wire.HarnessClaudeCode, Kind: wire.AIToolStart, Tool: "Edit"}},
		{Type: "activity", Activity: &wire.ActivitySample{Time: now, FocusKind: "editor"}},
		{Type: "git", Git: &wire.GitEvent{Time: now, Project: "app", Kind: "worktree", FilesChanged: 2}},
		{Type: "git", AI: &wire.AIEvent{}}, // mismatched envelope
	}
	token := func(v string) func(*http.Request) { return func(r *http.Request) { r.Header.Set(TokenHeader, v) } }
	tests := []struct {
		name   string
		body   any
		mutate func(*http.Request)
		want   int
	}{
		{"no token", good, func(*http.Request) {}, http.StatusUnauthorized},
		{"wrong token", good, token(strings.Repeat("0", 48)), http.StatusUnauthorized},
		{"not an array", `{"type":"ai"}`, token(testToken), http.StatusBadRequest},
		{"too many", make([]wire.LocalEvent, 501), token(testToken), http.StatusRequestEntityTooLarge},
		{"too large", `["` + strings.Repeat("x", 1<<20) + `"]`, token(testToken), http.StatusRequestEntityTooLarge},
		{"ok", good, token(testToken), http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/local/v1/events", tt.body, tt.mutate)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
	c, err := f.db.CountSince(context.Background(), now.Add(-time.Minute))
	if err != nil || c.AIEvents != 1 || c.ActivitySamples != 1 || c.GitEvents != 1 {
		t.Fatalf("counts = %+v, %v", c, err)
	}
}

func TestStatusBarProxyCachesAndFallsBack(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(testKey)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"grand_total":{"text":"3 hrs 1 min","total_seconds":10860}}}`))
	}))
	defer upstream.Close()
	f := newFixture(t, upstream.URL)
	clock := time.Now()
	f.nowFunc = func() time.Time { return clock }
	const path = "/api/v1/users/current/statusbar/today"

	for range 3 {
		rec := f.do(t, http.MethodGet, path, nil, basicKey(testKey))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "3 hrs 1 min") {
			t.Fatalf("proxy = %d %s", rec.Code, rec.Body)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (cached)", hits.Load())
	}

	// After the TTL, an upstream failure falls back to the local estimate.
	clock = clock.Add(time.Minute)
	fail.Store(true)
	rec := f.do(t, http.MethodGet, path, nil, basicKey(testKey))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "(offline)") {
		t.Fatalf("fallback = %d %s", rec.Code, rec.Body)
	}
	var sb struct {
		Data struct {
			GrandTotal struct {
				Text string `json:"text"`
			} `json:"grand_total"`
			Categories []any `json:"categories"`
			Projects   []any `json:"projects"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sb); err != nil || sb.Data.Projects == nil || len(sb.Data.Categories) != 1 {
		t.Fatalf("fallback shape = %s", rec.Body)
	}
}

func TestStatusBarOfflineEstimate(t *testing.T) {
	// Nothing listens on this address, so the proxy fails fast.
	f := newFixture(t, "http://127.0.0.1:1")
	base := time.Now().Add(-90 * time.Second)
	if base.Before(status.StartOfDay(time.Now())) {
		t.Skip("too close to midnight")
	}
	unix := func(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }
	hbs := []wire.Heartbeat{{Entity: "/a.go", Time: unix(base)}, {Entity: "/a.go", Time: unix(base.Add(time.Minute))}}
	if err := f.db.Enqueue(context.Background(), hbs, false); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, http.MethodGet, "/api/hackatime/v1/users/current/statusbar/today", nil, basicKey(testKey))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"1 min (offline)"`) {
		t.Fatalf("estimate = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodGet, "/api/v1/users/current/statusbar/today", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status bar = %d", rec.Code)
	}
}

func TestStatusEndpointsNeverLeakKeys(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1")
	for _, p := range []string{"/", "/local/v1/status"} {
		rec := f.do(t, http.MethodGet, p, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", p, rec.Code)
		}
		for _, secret := range []string{testKey, testToken, testHackatimeKey} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("%s leaks a secret", p)
			}
		}
	}
	if rec := f.do(t, http.MethodGet, "/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d", rec.Code)
	}
}
