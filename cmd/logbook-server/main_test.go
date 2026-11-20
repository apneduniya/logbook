package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/config"
	"github.com/apneduniya/logbook/internal/server/hackatime"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
)

// After the Hackatime consent screen the browser must land on the web UI,
// not on the API origin, which serves no pages (ADR 0011).
func TestHackatimeCallbackRedirectsToWebOrigin(t *testing.T) {
	cfg := config.Config{WebURL: "https://logbook.boogles.dev/"}
	mux := http.NewServeMux()
	// The declined path needs no service: it redirects before using one.
	hackatime.NewHandler(nil, httpapitest.StubSession, hackatimeHandlerOptions(cfg)).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpapitest.Request(t, http.MethodGet, "/auth/hackatime/callback?error=access_denied", uuid.New(), ""))
	want := "https://logbook.boogles.dev/settings?hackatime=denied"
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Fatalf("callback = %d %q, want 303 %q", rec.Code, rec.Header().Get("Location"), want)
	}
}
