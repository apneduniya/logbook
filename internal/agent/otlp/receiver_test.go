package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
)

const testToken = "59cd87cdb5c577d2173d433b577a1d803cfe94ca7b299b96492b7ff1c3a477c1"

type env struct {
	db   *localdb.DB
	path string
	rcv  *Receiver
	h    http.Handler
	logs *bytes.Buffer
}

func newEnv(t testing.TB) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logbook.db")
	db, err := localdb.Open(context.Background(), path, localdb.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{db: db, path: path, logs: &bytes.Buffer{}}
	e.restart()
	return e
}

// restart replaces the receiver, as an agent restart would; the database
// keeps the series state.
func (e *env) restart() {
	e.rcv = New(Options{
		Store: e.db, Token: func() string { return testToken },
		Logger: slog.New(slog.NewTextHandler(e.logs, nil)), Now: func() time.Time { return now },
	})
	e.h = e.rcv.Handler()
}

func (e *env) post(t testing.TB, path string, body []byte, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TokenHeader, testToken)
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// stored renders every stored point in [-2h, +1m) one per line.
func (e *env) stored(t *testing.T) string {
	t.Helper()
	ats, err := e.db.ActiveTimes(context.Background(), now.Add(-2*time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, a := range ats {
		fmt.Fprintf(&b, "%s %s..%s user=%g cli=%g\n", a.SessionID, a.From.Format(time.TimeOnly), a.To.Format(time.TimeOnly), a.UserSecs, a.CLISecs)
	}
	return b.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("LOGBOOK_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("reading golden (set LOGBOOK_UPDATE_GOLDEN=1 to create): %v", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("%s mismatch\n--- got ---\n%s--- want ---\n%s", path, got, want)
	}
}

func TestDeltaExport(t *testing.T) {
	e := newEnv(t)
	rec := e.post(t, "/v1/metrics", fixture(t, "delta.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// One datapoint (type "idle") was unusable: reported as a partial success.
	if got := strings.TrimSpace(rec.Body.String()); !strings.Contains(got, `"rejectedDataPoints":"1"`) {
		t.Fatalf("body = %s", got)
	}
	// A retried export changes nothing.
	if rec := e.post(t, "/v1/metrics", fixture(t, "delta.json")); rec.Code != http.StatusOK {
		t.Fatalf("retry status %d", rec.Code)
	}
	golden(t, "delta_stored", e.stored(t))
}

// A sequence of cumulative exports becomes increments: the first point counts
// from the series start, retries are skipped, a lower total is a reset, a new
// start time is a new series; an agent restart in the middle changes nothing.
func TestCumulativeExports(t *testing.T) {
	e := newEnv(t)
	for i, name := range []string{"cumulative_1.json", "cumulative_2.json", "cumulative_3.json", "cumulative_4.json"} {
		if i == 2 {
			e.restart()
		}
		rec := e.post(t, "/v1/metrics", fixture(t, name))
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
			t.Fatalf("%s: status %d: %s", name, rec.Code, rec.Body)
		}
		// Every export may be resent; nothing is counted twice.
		if rec := e.post(t, "/v1/metrics", fixture(t, name)); rec.Code != http.StatusOK {
			t.Fatalf("%s resent: status %d", name, rec.Code)
		}
	}
	golden(t, "cumulative_stored", e.stored(t))
}

// No identity attribute reaches the database: the sentinel planted in
// user.email, account ids, organization.id, user.id, host and terminal
// attributes appears nowhere in the database files.
func TestIdentityNeverStored(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"delta.json", "cumulative_1.json", "cumulative_2.json"} {
		if rec := e.post(t, "/v1/metrics", fixture(t, name)); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, rec.Code)
		}
	}
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(e.path + "*")
	if err != nil || len(matches) == 0 {
		t.Fatalf("no database files: %v", err)
	}
	for _, f := range matches {
		b, err := os.ReadFile(f) //nolint:gosec // test path
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("SENTINEL")) || bytes.Contains(b, []byte("example.com")) {
			t.Fatalf("identity attribute stored in %s", f)
		}
	}
	if strings.Contains(e.logs.String(), "SENTINEL") {
		t.Fatalf("identity attribute logged: %s", e.logs)
	}
}

func TestRejections(t *testing.T) {
	body := fixture(t, "delta.json")
	tests := []struct {
		name   string
		path   string
		body   []byte
		mutate func(*http.Request)
		want   int
	}{
		{"no token", "/v1/metrics", body, func(r *http.Request) { r.Header.Del(TokenHeader) }, http.StatusUnauthorized},
		{"wrong token", "/v1/metrics", body, func(r *http.Request) { r.Header.Set(TokenHeader, testToken[1:]+"0") }, http.StatusUnauthorized},
		{"local token is not the OTLP token", "/v1/metrics", body, func(r *http.Request) { r.Header.Set(TokenHeader, strings.Repeat("a", 64)) }, http.StatusUnauthorized},
		{"protobuf", "/v1/metrics", []byte{0x0a, 0x00}, func(r *http.Request) { r.Header.Set("Content-Type", "application/x-protobuf") }, http.StatusUnsupportedMediaType},
		{"no content type", "/v1/metrics", body, func(r *http.Request) { r.Header.Del("Content-Type") }, http.StatusUnsupportedMediaType},
		{"json with charset", "/v1/metrics", body, func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }, http.StatusOK},
		{"deflate", "/v1/metrics", body, func(r *http.Request) { r.Header.Set("Content-Encoding", "deflate") }, http.StatusUnsupportedMediaType},
		{"bad gzip", "/v1/metrics", body, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, http.StatusBadRequest},
		{"bad json", "/v1/metrics", []byte(`{"resourceMetrics":`), nil, http.StatusBadRequest},
		{"too big", "/v1/metrics", bytes.Repeat([]byte(" "), MaxBody+1), nil, http.StatusRequestEntityTooLarge},
		{"exactly the limit", "/v1/metrics", append([]byte(`{}`), bytes.Repeat([]byte(" "), MaxBody-2)...), nil, http.StatusOK},
		{"gzip bomb", "/v1/metrics", gz(t, bytes.Repeat([]byte(" "), 4*MaxBody)), func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, http.StatusRequestEntityTooLarge},
		{"logs need the token", "/v1/logs", []byte(`{}`), func(r *http.Request) { r.Header.Del(TokenHeader) }, http.StatusUnauthorized},
		{"logs are discarded", "/v1/logs", []byte(`{"resourceLogs":[]}`), nil, http.StatusOK},
		{"logs protobuf too", "/v1/logs", []byte{0x0a}, func(r *http.Request) { r.Header.Set("Content-Type", "application/x-protobuf") }, http.StatusOK},
		{"logs too big", "/v1/logs", bytes.Repeat([]byte(" "), MaxBody+1), nil, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			var m []func(*http.Request)
			if tc.mutate != nil {
				m = append(m, tc.mutate)
			}
			rec := e.post(t, tc.path, tc.body, m...)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
			if tc.want != http.StatusOK && !strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("error body = %s", rec.Body)
			}
			if tc.want != http.StatusOK || tc.path == "/v1/logs" {
				if got := e.stored(t); got != "" {
					t.Fatalf("stored after a rejected or discarded request:\n%s", got)
				}
			}
		})
	}
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGzipAccepted(t *testing.T) {
	e := newEnv(t)
	rec := e.post(t, "/v1/metrics", gz(t, fixture(t, "delta.json")), func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") })
	if rec.Code != http.StatusOK || e.stored(t) == "" {
		t.Fatalf("status %d, stored %q", rec.Code, e.stored(t))
	}
}

// Without a token (no config yet) every request is refused.
func TestNoTokenRefusesAll(t *testing.T) {
	e := newEnv(t)
	e.rcv = New(Options{Store: e.db, Token: func() string { return "" }})
	e.h = e.rcv.Handler()
	rec := e.post(t, "/v1/metrics", fixture(t, "delta.json"), func(r *http.Request) { r.Header.Set(TokenHeader, "") })
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
}

// The protobuf warning is logged once, not on every export.
func TestProtobufWarnsOnce(t *testing.T) {
	e := newEnv(t)
	for range 3 {
		e.post(t, "/v1/metrics", []byte{0x0a}, func(r *http.Request) { r.Header.Set("Content-Type", "application/x-protobuf") })
	}
	if n := strings.Count(e.logs.String(), "protobuf"); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, e.logs)
	}
}

type failingStore struct{ Store }

func (failingStore) InsertActiveTime(context.Context, []localdb.ActiveTimePoint) error {
	return errors.New("disk full")
}

// A storage failure answers 503, which OTLP exporters retry.
func TestStoreFailureIsRetryable(t *testing.T) {
	e := newEnv(t)
	e.rcv = New(Options{Store: failingStore{e.db}, Token: func() string { return testToken }, Now: func() time.Time { return now }})
	e.h = e.rcv.Handler()
	if rec := e.post(t, "/v1/metrics", fixture(t, "delta.json")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestOnlyPost(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d", rec.Code)
	}
}
