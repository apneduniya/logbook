package httpapi_test

import (
	"bytes"
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
