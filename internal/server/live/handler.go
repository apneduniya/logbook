package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// PublicLookup resolves a username to a user id when, and only when, the
// profile is public. profiles.Service implements it.
type PublicLookup interface {
	PublicUserID(ctx context.Context, username string) (id uuid.UUID, ok bool, err error)
}

// Config tunes the SSE streams. Zero fields take the defaults below.
type Config struct {
	// Keepalive is how often a comment line is sent to keep proxies from
	// closing an idle stream.
	Keepalive time.Duration
	// MaxDuration ends a stream after this long; EventSource reconnects,
	// which re-runs authentication so a revoked session stops streaming.
	MaxDuration time.Duration
	// WriteTimeout bounds each write so a stalled client can't pin a handler.
	WriteTimeout time.Duration
	// Retry is the reconnect delay suggested to the browser.
	Retry time.Duration
}

func (c Config) withDefaults() Config {
	if c.Keepalive <= 0 {
		c.Keepalive = 25 * time.Second
	}
	if c.MaxDuration <= 0 {
		c.MaxDuration = 30 * time.Minute
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.Retry <= 0 {
		c.Retry = 5 * time.Second
	}
	return c
}

// Handler serves the live SSE endpoints.
type Handler struct {
	hub            *Hub
	lookup         PublicLookup
	requireSession httpapi.Middleware
	cfg            Config
}

// NewHandler returns a Handler; requireSession must authenticate the caller.
func NewHandler(hub *Hub, lookup PublicLookup, requireSession httpapi.Middleware, cfg Config) *Handler {
	return &Handler{hub: hub, lookup: lookup, requireSession: requireSession, cfg: cfg.withDefaults()}
}

// Register mounts the live routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/me/live", h.requireSession(httpapi.Handle(h.me)))
	mux.Handle("GET /api/v1/users/{username}/live", httpapi.Handle(h.public))
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	sub, err := h.subscribe(u.ID, false)
	if err != nil {
		return err
	}
	h.stream(w, r, sub, false)
	return nil
}

func (h *Handler) public(w http.ResponseWriter, r *http.Request) error {
	id, ok, err := h.lookup.PublicUserID(r.Context(), r.PathValue("username"))
	if err != nil {
		return fmt.Errorf("looking up profile: %w", err)
	}
	if !ok {
		return httpapi.Errorf(http.StatusNotFound, nil, "not found")
	}
	sub, err := h.subscribe(id, true)
	if err != nil {
		return err
	}
	h.stream(w, r, sub, true)
	return nil
}

func (h *Handler) subscribe(userID uuid.UUID, public bool) (*Subscription, error) {
	sub, err := h.hub.Subscribe(userID, public)
	switch {
	case errors.Is(err, ErrUserLimit):
		return nil, httpapi.Errorf(http.StatusTooManyRequests, err, "too many live streams open")
	case errors.Is(err, ErrGlobalLimit), errors.Is(err, ErrClosed):
		return nil, httpapi.Errorf(http.StatusServiceUnavailable, err, "live updates unavailable, try again later")
	case err != nil:
		return nil, fmt.Errorf("subscribing: %w", err)
	}
	return sub, nil
}

// minutesData is the owner stream's event payload.
type minutesData struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// stream writes SSE until the client leaves, the hub ends the subscription
// or MaxDuration passes. It always closes sub.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, sub *Subscription, public bool) {
	defer sub.Close()
	ctx := r.Context()
	rc := http.NewResponseController(w)

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no") // stop nginx-style proxies buffering the stream
	w.WriteHeader(http.StatusOK)

	write := func(s string) bool {
		// Deadlines are best-effort: test recorders don't support them.
		if err := rc.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		//nolint:gosec // text/event-stream of server-built frames and JSON, never rendered as HTML
		if _, err := fmt.Fprint(w, s); err != nil {
			return false
		}
		if err := rc.Flush(); err != nil {
			slog.DebugContext(ctx, "live stream flush failed", "err", err)
			return false
		}
		return true
	}

	if !write(fmt.Sprintf("retry: %d\nevent: ready\ndata: {}\n\n", h.cfg.Retry.Milliseconds())) {
		return
	}
	keepalive := time.NewTicker(h.cfg.Keepalive)
	defer keepalive.Stop()
	deadline := time.NewTimer(h.cfg.MaxDuration)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			return
		case <-deadline.C:
			return
		case <-keepalive.C:
			if !write(": keepalive\n\n") {
				return
			}
		case <-sub.Notify():
			ev, ok := sub.Take()
			if !ok {
				continue
			}
			msg, err := formatEvent(ev, public)
			if err != nil {
				slog.WarnContext(ctx, "dropping live event", "err", err)
				continue
			}
			if !write(msg) {
				return
			}
		}
	}
}

// formatEvent renders an SSE "minutes" event. Public streams get no range so
// viewers learn nothing beyond "the profile changed".
func formatEvent(ev Event, public bool) (string, error) {
	data := []byte("{}")
	if !public {
		var err error
		if data, err = json.Marshal(minutesData(ev)); err != nil {
			return "", fmt.Errorf("encoding event: %w", err)
		}
	}
	return "event: minutes\ndata: " + string(data) + "\n\n", nil
}
