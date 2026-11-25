package httpapi_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// A path whose route has a {token} segment is logged as its pattern: the
// token is a credential (review links, ADR 0020). Other paths are logged
// as they are.
func TestLoggerKeepsTokensOutOfLogs(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mux := http.NewServeMux()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("GET /api/v1/review/{token}", ok)
	mux.Handle("GET /api/v1/me/submissions/{id}", ok)
	h := httpapi.Logger(mux)

	for _, path := range []string{"/api/v1/review/s3cretT0kenValue", "/api/v1/me/submissions/1234"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	}
	out := buf.String()
	if strings.Contains(out, "s3cretT0kenValue") || !strings.Contains(out, "path=/api/v1/review/{token}") {
		t.Fatalf("review path logged as:\n%s", out)
	}
	if !strings.Contains(out, "path=/api/v1/me/submissions/1234") {
		t.Fatalf("other path logged as:\n%s", out)
	}
}

// The token stays out of every log line with the server's middleware
// order (router.Handler), on requests no route answers too: a CORS
// preflight (answered before the mux), a 5xx (WriteError's line), a 404, a
// 405, a path in another case and one the mux redirects.
func TestTokensStayOutOfLogsOnEveryAnswer(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secret = "s3cretT0kenValueXXXXXX"
	const origin = "https://web.test"
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/review/{token}", httpapi.Handle(func(http.ResponseWriter, *http.Request) error {
		return errors.New("database down")
	}))
	mux.Handle("POST /api/v1/review/{token}/decision", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h := httpapi.Chain(mux, httpapi.Recover, httpapi.RequestID, httpapi.Logger, httpapi.SecurityHeaders,
		httpapi.CORS(httpapi.CORSConfig{Origin: origin}))

	preflight := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/api/v1/review/"+secret+"/decision", nil)
	preflight.Header.Set("Origin", origin)
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type, x-csrf-token")
	reqs := []*http.Request{preflight}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/review/" + secret},                // 500
		{http.MethodGet, "/api/v1/review/" + secret + "/nothing"},   // 404
		{http.MethodDelete, "/api/v1/review/" + secret},             // 405
		{http.MethodGet, "/API/V1/Review/" + secret},                // 404
		{http.MethodGet, "/api/v1//review/" + secret + "/decision"}, // redirect
	} {
		reqs = append(reqs, httptest.NewRequestWithContext(t.Context(), c.method, c.path, nil))
	}
	for _, r := range reqs {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("the token is in the log:\n%s", out)
	}
	for _, want := range []string{
		"method=OPTIONS path=/api/v1/review/{token}/decision status=204",
		`msg="request failed" err="database down" path=/api/v1/review/{token}`,
		"path=/api/v1/review/{token}/nothing status=404",
		"method=DELETE path=/api/v1/review/{token} status=405",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log has no %q:\n%s", want, out)
		}
	}
}
