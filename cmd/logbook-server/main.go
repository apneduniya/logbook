// Command logbook-server runs the Logbook API and background jobs.
// It only wires dependencies together; behaviour lives in internal/server.
//
// `logbook-server rebuild-rollups` recomputes every user's raw per-minute
// rollups from stored heartbeats and exits.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for migrations
	"github.com/riverqueue/river"

	"github.com/apneduniya/logbook/internal/server/auth"
	"github.com/apneduniya/logbook/internal/server/config"
	"github.com/apneduniya/logbook/internal/server/credited"
	"github.com/apneduniya/logbook/internal/server/devices"
	"github.com/apneduniya/logbook/internal/server/hackatime"
	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/ingest"
	"github.com/apneduniya/logbook/internal/server/jobs"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/live"
	"github.com/apneduniya/logbook/internal/server/profiles"
	"github.com/apneduniya/logbook/internal/server/projects"
	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/stats"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	logger := setupLogging(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := migrate(ctx, cfg.DatabaseURL.Reveal()); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := jobs.Migrate(ctx, st.Pool); err != nil {
		return fmt.Errorf("migrating job tables: %w", err)
	}

	key, err := cfg.EncryptionKeyBytes()
	if err != nil {
		return err
	}
	box, err := secretbox.New(key)
	if err != nil {
		return fmt.Errorf("building secretbox: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpapi.Handle(func(w http.ResponseWriter, r *http.Request) error {
		if err := st.Pool.Ping(r.Context()); err != nil {
			return httpapi.Errorf(http.StatusServiceUnavailable, err, "database unavailable")
		}
		httpapi.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))

	// Accounts and sessions.
	authSvc, err := auth.New(st, auth.Options{
		BaseURL: cfg.BaseURL,
		WebURL:  cfg.WebURL,
		HCA: auth.HCAConfig{
			BaseURL:      cfg.HCA.BaseURL,
			ClientID:     cfg.HCA.ClientID,
			ClientSecret: cfg.HCA.ClientSecret.Reveal(),
		},
		SecretKey:       key,
		SessionTTL:      cfg.SessionTTL,
		InsecureCookies: cfg.DevInsecureCookie,
		DevLogin:        cfg.DevLogin,
		TrustProxy:      cfg.TrustProxy,
	})
	if err != nil {
		return fmt.Errorf("building auth: %w", err)
	}
	auth.NewHandler(authSvc).Register(mux)
	go authSvc.RunSessionCleanup(ctx, time.Hour)

	// Stats, profiles, projects and live updates.
	publicLimit := httpapi.NewRateLimiter(120.0/60, 120, 10_000, 10*time.Minute).Middleware(httpapi.IPKey(cfg.TrustProxy))
	hub := live.NewHub(live.DefaultLimits())
	statsSvc := stats.NewService(st)
	profileSvc := profiles.NewService(st, hub)
	projectSvc := projects.NewService(st, profileSvc)
	stats.NewHandler(statsSvc, authSvc.RequireSession).Register(mux)
	projects.NewHandler(projectSvc, authSvc.RequireSession).Register(mux)
	profiles.NewHandler(profileSvc, authSvc.RequireSession, publicLimit).Register(mux)
	live.NewHandler(hub, profileSvc, authSvc.RequireSession, publicLimit, live.Config{
		ClientKey: httpapi.IPKey(cfg.TrustProxy),
	}).Register(mux)

	// Heartbeat ingest. The streak cache is refreshed before the hub tells browsers to refetch.
	ingestSvc := ingest.New(st, live.Fanout{statsSvc, hub})
	if len(os.Args) > 1 && os.Args[1] == "rebuild-rollups" {
		// One-off maintenance: recompute every user's raw per-minute rollups
		// from stored heartbeats (after a duration-rule change), then exit.
		n, err := ingestSvc.RebuildAll(ctx)
		logger.Info("rebuilt rollups", "users", n, "err", err)
		return err
	}
	ingest.NewHandler(ingestSvc, authSvc.RequireAPIKey).Register(mux)

	// Agent devices, credited minutes and the evidence chain (ADR 0010).
	devices.NewHandler(devices.NewService(st), authSvc.RequireAPIKey, authSvc.RequireSession).Register(mux)
	credited.NewHandler(credited.NewService(st, hub), authSvc.RequireAPIKey).Register(mux)
	ledger.NewHandler(ledger.NewService(st), authSvc.RequireAPIKey).Register(mux)

	// Hackatime link, history import and trust refresh.
	htClient, err := hackatime.NewClient(hackatime.ClientConfig{
		BaseURL:      cfg.Hackatime.BaseURL,
		ClientID:     cfg.Hackatime.ClientID,
		ClientSecret: cfg.Hackatime.ClientSecret.Reveal(),
		RedirectURL:  strings.TrimRight(cfg.BaseURL, "/") + "/auth/hackatime/callback",
	})
	if err != nil {
		return fmt.Errorf("building hackatime client: %w", err)
	}
	workers := river.NewWorkers()
	if err := hackatime.AddWorkers(workers,
		hackatime.NewImporter(st, box, htClient, ingestSvc, hackatime.ImportOptions{Logger: logger}),
		hackatime.NewTrustRefresher(st, box, htClient, logger, nil),
	); err != nil {
		return fmt.Errorf("registering workers: %w", err)
	}
	jobClient, err := jobs.New(st.Pool, jobs.Options{
		Workers:      workers,
		PeriodicJobs: hackatime.PeriodicJobs(),
		Queues:       map[string]int{hackatime.Queue: 2},
		Logger:       logger,
	})
	if err != nil {
		return fmt.Errorf("building job client: %w", err)
	}
	htSvc := hackatime.NewService(st, box, htClient, jobClient.River(), hackatime.ServiceOptions{Logger: logger})
	hackatime.NewHandler(htSvc, authSvc.RequireSession, hackatimeHandlerOptions(cfg)).Register(mux)

	if err := jobClient.Start(ctx); err != nil {
		return fmt.Errorf("starting jobs: %w", err)
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: httpapi.Chain(mux, httpapi.Recover, httpapi.RequestID, httpapi.Logger, httpapi.SecurityHeaders,
			httpapi.CORS(httpapi.CORSConfig{Origin: cfg.WebOrigin(), Skip: corsSkip})),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE streams are long-lived; handlers set their own deadlines.
	}
	srv.RegisterOnShutdown(hub.Close) // close SSE streams so Shutdown doesn't wait on them

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	var serveErr error
	select {
	case err := <-errCh:
		serveErr = fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		serveErr = errors.Join(serveErr, fmt.Errorf("shutting down http: %w", err))
	}
	if err := jobClient.Stop(shutdownCtx); err != nil {
		serveErr = errors.Join(serveErr, fmt.Errorf("stopping jobs: %w", err))
	}
	return serveErr
}

// hackatimeHandlerOptions sends the browser back to the web UI's settings
// page after linking Hackatime: the API origin serves no pages (ADR 0011).
func hackatimeHandlerOptions(cfg config.Config) hackatime.HandlerOptions {
	return hackatime.HandlerOptions{
		InsecureCookies: cfg.DevInsecureCookie,
		SettingsPath:    cfg.WebOrigin() + "/settings",
	}
}

// corsSkip lists the /api/ paths browsers never call (wakatime-cli and agent
// endpoints, authenticated by API key), which get no CORS headers (ADR 0011).
var corsSkip = []string{
	"/api/hackatime/",
	"/api/v1/users/current/heartbeats",
	"/api/v1/users/current/statusbar/",
	"/api/v1/devices",
}

func migrate(ctx context.Context, databaseURL string) error {
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("opening migration connection: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()
	return migrations.Up(ctx, sqlDB)
}

func setupLogging(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
	slog.SetDefault(logger)
	return logger
}
