package localapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localkey"
	"github.com/apneduniya/logbook/internal/agent/otlp"
)

// The OTLP endpoints sit behind the same loopback, Host and cross-site
// checks as every local endpoint, and need the OTLP token.
func TestOTLPMountedBehindGuard(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1")
	otlpToken, err := localkey.OTLPToken(testToken)
	if err != nil {
		t.Fatal(err)
	}
	rcv := otlp.New(otlp.Options{Store: f.db, Token: func() string { return otlpToken }})
	srv := New(Options{
		Settings: func() config.Config { return f.cfg }, Store: f.db, OTLP: rcv.Handler(),
		Status: f.srv.opt.Status, Page: f.srv.opt.Page, Client: f.srv.opt.Client,
	})
	h := srv.Handler(testPort)
	body := []byte(`{"resourceMetrics":[]}`)
	tests := []struct {
		name   string
		path   string
		mutate func(*http.Request)
		want   int
	}{
		{"metrics", "/v1/metrics", func(*http.Request) {}, http.StatusOK},
		{"logs", "/v1/logs", func(*http.Request) {}, http.StatusOK},
		{"local token is refused", "/v1/metrics", func(r *http.Request) { r.Header.Set(otlp.TokenHeader, testToken) }, http.StatusUnauthorized},
		{"remote addr", "/v1/metrics", func(r *http.Request) { r.RemoteAddr = "10.0.0.5:4000" }, http.StatusForbidden},
		{"rebinding host", "/v1/metrics", func(r *http.Request) { r.Host = "evil.example:47231" }, http.StatusForbidden},
		{"cross-site", "/v1/metrics", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
		{"foreign origin", "/v1/logs", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, tc.path, bytes.NewReader(body))
			req.RemoteAddr = "127.0.0.1:50000"
			req.Host = "127.0.0.1:47231"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(otlp.TokenHeader, otlpToken)
			tc.mutate(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// Without an OTLP handler the routes don't exist.
func TestOTLPNotMountedByDefault(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1")
	rec := f.do(t, http.MethodPost, "/v1/metrics", `{}`)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}
