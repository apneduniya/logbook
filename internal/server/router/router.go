package router

import (
	"context"
	"net/http"
	"time"

	"github.com/apneduniya/logbook/internal/server/auth"
	"github.com/apneduniya/logbook/internal/server/credited"
	"github.com/apneduniya/logbook/internal/server/devices"
	"github.com/apneduniya/logbook/internal/server/hackatime"
	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/ingest"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/live"
	"github.com/apneduniya/logbook/internal/server/oauth"
	"github.com/apneduniya/logbook/internal/server/profiles"
	"github.com/apneduniya/logbook/internal/server/programapi"
	"github.com/apneduniya/logbook/internal/server/projects"
	"github.com/apneduniya/logbook/internal/server/stats"
)

// Services are the built services whose routes the API serves. Every field
// is required.
type Services struct {
	// Ping checks the database for GET /healthz.
	Ping       func(ctx context.Context) error
	Auth       *auth.Service
	OAuth      *oauth.Service
	Stats      *stats.Service
	Profiles   *profiles.Service
	Projects   *projects.Service
	Hub        *live.Hub
	Ingest     *ingest.Service
	Devices    *devices.Service
	Credited   *credited.Service
	Ledger     *ledger.Service
	Hackatime  *hackatime.Service
	ProgramAPI *programapi.Service
}

// Options configures the handler.
type Options struct {
	// WebOrigin is the web UI's origin, the only one CORS allows.
	WebOrigin string
	// TrustProxy keys per-client rate limits on the proxy-set
	// X-Forwarded-For hop instead of the peer address.
	TrustProxy bool
	// Hackatime configures the Hackatime link routes.
	Hackatime hackatime.HandlerOptions
	// Live tunes the SSE streams; its ClientKey is set here.
	Live live.Config
}

// Public read budget per client (profiles, live streams, the program list):
// a burst of 120, refilled at 120 a minute.
const publicBurst = 120

// New mounts every route and returns the server's handler.
func New(s Services, opts Options) http.Handler {
	return httpapi.Chain(Mux(s, opts), httpapi.Recover, httpapi.RequestID, httpapi.Logger, httpapi.SecurityHeaders,
		httpapi.CORS(httpapi.CORSConfig{Origin: opts.WebOrigin, Skip: corsSkip}))
}

// Mux mounts every route on a new mux, without the global middleware. It
// panics, as http.ServeMux does, if two route patterns conflict.
func Mux(s Services, opts Options) *http.ServeMux {
	clientKey := httpapi.IPKey(opts.TrustProxy)
	publicLimit := httpapi.NewRateLimiter(publicBurst/60.0, publicBurst, 10_000, 10*time.Minute).Middleware(clientKey)
	requireSession, requireAPIKey := s.Auth.RequireSession, s.Auth.RequireAPIKey

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpapi.Handle(func(w http.ResponseWriter, r *http.Request) error {
		if err := s.Ping(r.Context()); err != nil {
			return httpapi.Errorf(http.StatusServiceUnavailable, err, "database unavailable")
		}
		httpapi.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))

	// Accounts and sessions.
	auth.NewHandler(s.Auth).Register(mux)

	// Stats, profiles, projects and live updates for the web UI.
	stats.NewHandler(s.Stats, requireSession).Register(mux)
	projects.NewHandler(s.Projects, requireSession).Register(mux)
	profiles.NewHandler(s.Profiles, requireSession, publicLimit).Register(mux)
	liveCfg := opts.Live
	liveCfg.ClientKey = clientKey
	live.NewHandler(s.Hub, s.Profiles, requireSession, publicLimit, liveCfg).Register(mux)

	// OAuth provider: apps (programs), consent, tokens and grants (ADR 0021).
	oauth.NewHandler(s.OAuth, requireSession, s.Auth.OptionalSession, publicLimit,
		oauth.HandlerOptions{TrustProxy: opts.TrustProxy}).Register(mux)

	// The Hackatime-shaped program API (ADR 0021). It reads OAuth access
	// tokens and, where Hackatime takes its own key, user API keys.
	programapi.NewHandler(s.ProgramAPI, ProgramCredentials{OAuth: s.OAuth, Keys: s.Auth},
		programapi.Options{ClientKey: clientKey}).Register(mux)

	// Heartbeat ingest, agent devices, credited minutes and the evidence
	// chain (ADR 0010).
	ingest.NewHandler(s.Ingest, requireAPIKey).Register(mux)
	devices.NewHandler(s.Devices, requireAPIKey, requireSession).Register(mux)
	credited.NewHandler(s.Credited, requireAPIKey).Register(mux)
	ledger.NewHandler(s.Ledger, requireAPIKey).Register(mux)

	// Hackatime link, history import and trust refresh.
	hackatime.NewHandler(s.Hackatime, requireSession, opts.Hackatime).Register(mux)
	return mux
}

// corsSkip lists the /api/ paths browsers never call, which get no CORS
// headers (ADR 0011): wakatime-cli and agent endpoints (API keys) and the
// program API, which program backends call with OAuth tokens or API keys.
// The web UI's own routes under /api/v1/users/{username}/ (profile, live)
// keep CORS.
var corsSkip = []string{
	"/api/hackatime/",
	"/api/v1/users/current/heartbeats",
	"/api/v1/users/current/statusbar/",
	"/api/v1/devices",
	"/api/v1/authenticated/",
	"/api/v1/my/",
	"/api/v1/users/{id}/stats",
	"/api/v1/users/{id}/trust_factor",
	"/api/v1/users/{id}/heartbeats",
	"/api/v1/users/{id}/projects",
	"/api/v1/users/{id}/project",
}
