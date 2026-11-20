package localapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/internal/agent/statuspage"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Limits on request bodies.
const (
	maxBodyBytes    = 1 << 20
	maxLocalEvents  = 500
	shutdownTimeout = 5 * time.Second
)

// TokenHeader carries the local token on POST /local/v1/events.
const TokenHeader = "X-Logbook-Token" //nolint:gosec // a header name, not a credential

// Store is the storage the server writes to. *localdb.DB satisfies it.
type Store interface {
	Enqueue(ctx context.Context, hbs []wire.Heartbeat, mirror bool) error
	InsertEvents(ctx context.Context, evs []wire.LocalEvent, marks ...localdb.KV) error
	HeartbeatTimes(ctx context.Context, since time.Time) ([]time.Time, error)
}

// Options configures a Server.
type Options struct {
	// Settings returns the current config; it is read on every request so
	// a new login applies without a restart.
	Settings func() config.Config
	Store    Store
	// Status builds the snapshot for GET / and GET /local/v1/status.
	Status func(ctx context.Context) (status.Snapshot, error)
	Page   *statuspage.Renderer
	// Client is used for the status-bar proxy; it must have a timeout.
	Client    *http.Client
	UserAgent string
	// OnEnqueue is called after heartbeats are queued (to wake uploaders).
	OnEnqueue func()
	// MirrorEnabled decides whether heartbeats also go to the mirror queue.
	MirrorEnabled func(config.Config) bool
	Logger        *slog.Logger
	Now           func() time.Time
	// StatusBarTimeout and StatusBarTTL tune the proxy (defaults 3 s and 30 s).
	StatusBarTimeout, StatusBarTTL time.Duration
	// OTLP, if set, serves POST /v1/metrics and POST /v1/logs (Claude Code's
	// OpenTelemetry exporter; see package otlp). It authenticates requests
	// itself; the loopback, Host and cross-site checks apply as everywhere.
	OTLP http.Handler
}

// Server is the loopback HTTP server.
type Server struct {
	opt Options
	sb  *statusBar
}

// New returns a Server.
func New(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.OnEnqueue == nil {
		o.OnEnqueue = func() {}
	}
	if o.MirrorEnabled == nil {
		o.MirrorEnabled = func(config.Config) bool { return false }
	}
	if o.StatusBarTimeout <= 0 {
		o.StatusBarTimeout = 3 * time.Second
	}
	if o.StatusBarTTL <= 0 {
		o.StatusBarTTL = 30 * time.Second
	}
	return &Server{opt: o, sb: newStatusBar(o)}
}

// Handler returns the routes, guarded for a server listening on port.
func (s *Server) Handler(port int) http.Handler {
	mux := http.NewServeMux()
	for _, p := range []struct {
		prefix       string
		singleStatus int
	}{
		// WakaTime answers a single heartbeat with 201, Hackatime with 202.
		{"/api/v1", http.StatusCreated},
		{"/api/hackatime/v1", http.StatusAccepted},
	} {
		mux.HandleFunc("POST "+p.prefix+"/users/current/heartbeats", s.single(p.singleStatus))
		mux.HandleFunc("POST "+p.prefix+"/users/current/heartbeats.bulk", s.bulk)
		mux.HandleFunc("GET "+p.prefix+"/users/current/statusbar/today", s.statusBar)
	}
	mux.HandleFunc("POST /local/v1/events", s.events)
	mux.HandleFunc("GET /local/v1/status", s.statusJSON)
	mux.HandleFunc("GET /{$}", s.statusPage)
	if s.opt.OTLP != nil {
		mux.Handle("POST /v1/metrics", s.opt.OTLP)
		mux.Handle("POST /v1/logs", s.opt.OTLP)
	}
	return guard(port, mux)
}

// Serve serves on ln until ctx is done, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("local API needs a TCP listener, got %T", ln.Addr())
	}
	srv := &http.Server{
		Handler:           s.Handler(addr.Port),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.opt.Logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("local API: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("shutting down local API: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("local API: %w", err)
	}
	return nil
}

// guard rejects requests that aren't from this machine or that a browser
// sent on behalf of another site.
func guard(port int, next http.Handler) http.Handler {
	p := strconv.Itoa(port)
	hosts := []string{"127.0.0.1:" + p, "localhost:" + p}
	origins := []string{"http://127.0.0.1:" + p, "http://localhost:" + p}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			writeError(w, http.StatusForbidden, "loopback only")
			return
		}
		if !matchesAny(r.Host, hosts) {
			writeError(w, http.StatusForbidden, "bad host")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !matchesAny(o, origins) {
			writeError(w, http.StatusForbidden, "cross-origin requests are not allowed")
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			writeError(w, http.StatusForbidden, "cross-site requests are not allowed")
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func matchesAny(v string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(v, a) {
			return true
		}
	}
	return false
}

// equalSecret compares secrets in constant time. An empty expected value
// never matches.
func equalSecret(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client hung up; nothing to do
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) statusJSON(w http.ResponseWriter, r *http.Request) {
	snap, err := s.opt.Status(r.Context())
	if err != nil {
		s.opt.Logger.ErrorContext(r.Context(), "building status", "err", err)
		writeError(w, http.StatusInternalServerError, "could not read status")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	snap, err := s.opt.Status(r.Context())
	if err != nil {
		s.opt.Logger.ErrorContext(r.Context(), "building status", "err", err)
		http.Error(w, "could not read status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := s.opt.Page.Render(w, snap); err != nil {
		s.opt.Logger.ErrorContext(r.Context(), "rendering status page", "err", err)
	}
}
