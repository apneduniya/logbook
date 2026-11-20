// Command logbook-server runs the Logbook API, web UI and background jobs.
// It only wires dependencies together; behaviour lives in internal/server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/apneduniya/logbook/internal/server/config"
	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store"
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
	setupLogging(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer st.Close()

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpapi.Handle(func(w http.ResponseWriter, r *http.Request) error {
		if err := st.Pool.Ping(r.Context()); err != nil {
			return httpapi.Errorf(http.StatusServiceUnavailable, err, "database unavailable")
		}
		httpapi.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))
	// Feature modules register their routes here (auth, ingest, hackatime, stats, live, web).

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.Chain(mux, httpapi.Recover, httpapi.RequestID, httpapi.Logger, httpapi.SecurityHeaders),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE streams are long-lived; handlers set their own deadlines.
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}
