package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// PublicProfiles answers what public profile streams need to know.
// profiles.Service implements it.
type PublicProfiles interface {
	// PublicUserID resolves a username to a user id when, and only when,
	// the profile is public.
	PublicUserID(ctx context.Context, username string) (id uuid.UUID, ok bool, err error)
	// PublicVersion fingerprints what userID's public profile shows, from
	// data read no earlier than freshAfter (zero accepts any cached value).
	// ok is false when the profile is no longer public.
	PublicVersion(ctx context.Context, userID uuid.UUID, freshAfter time.Time) (version string, ok bool, err error)
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
	// PublicInterval is the shortest gap between two checks (and so two
	// events) on a public stream. It bounds how often viewers refetch.
	PublicInterval time.Duration
	// CheckTimeout bounds each PublicVersion call.
	CheckTimeout time.Duration
	// ClientKey identifies the client of a public stream for the per-client
	// cap. Pass httpapi.IPKey(trustProxy) behind a reverse proxy. The default
	// is the connection's remote IP.
	ClientKey func(*http.Request) string
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
	if c.PublicInterval <= 0 {
		c.PublicInterval = time.Minute
	}
	if c.CheckTimeout <= 0 {
		c.CheckTimeout = 5 * time.Second
	}
	if c.ClientKey == nil {
		c.ClientKey = remoteIP
	}
	return c
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Handler serves the live SSE endpoints.
type Handler struct {
	hub            *Hub
	profiles       PublicProfiles
	requireSession httpapi.Middleware
	publicLimit    httpapi.Middleware
	cfg            Config
}

// NewHandler returns a Handler. requireSession must authenticate the caller;
// publicLimit rate-limits the unauthenticated public stream per client.
func NewHandler(hub *Hub, profiles PublicProfiles, requireSession, publicLimit httpapi.Middleware, cfg Config) *Handler {
	return &Handler{
		hub: hub, profiles: profiles,
		requireSession: requireSession, publicLimit: publicLimit,
		cfg: cfg.withDefaults(),
	}
}

// Register mounts the live routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/me/live", h.requireSession(httpapi.Handle(h.me)))
	mux.Handle("GET /api/v1/users/{username}/live", h.publicLimit(httpapi.Handle(h.public)))
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	sub, err := h.hub.SubscribeOwner(u.ID)
	if err != nil {
		return subscribeError(err)
	}
	defer sub.Close()
	h.ownerLoop(newStream(w, r, h.cfg), sub)
	return nil
}

func (h *Handler) public(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, ok, err := h.profiles.PublicUserID(ctx, r.PathValue("username"))
	if err != nil {
		return fmt.Errorf("looking up profile: %w", err)
	}
	if !ok {
		return httpapi.Errorf(http.StatusNotFound, nil, "not found")
	}
	sub, err := h.hub.SubscribePublic(id, h.cfg.ClientKey(r))
	if err != nil {
		return subscribeError(err)
	}
	defer sub.Close()
	// Check visibility again now that the subscription exists: a profile
	// made private between the lookup and Subscribe would otherwise miss
	// ProfileMadePrivate and keep streaming.
	version, ok, err := h.version(ctx, id, time.Time{})
	if err != nil {
		return err
	}
	if !ok {
		return httpapi.Errorf(http.StatusNotFound, nil, "not found")
	}
	h.publicLoop(newStream(w, r, h.cfg), sub, id, version)
	return nil
}

func subscribeError(err error) error {
	switch {
	case errors.Is(err, ErrUserLimit), errors.Is(err, ErrClientLimit):
		return httpapi.Errorf(http.StatusTooManyRequests, err, "too many live streams open")
	case errors.Is(err, ErrGlobalLimit), errors.Is(err, ErrClosed):
		return httpapi.Errorf(http.StatusServiceUnavailable, err, "live updates unavailable, try again later")
	}
	return fmt.Errorf("subscribing: %w", err)
}

func (h *Handler) version(ctx context.Context, userID uuid.UUID, freshAfter time.Time) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.CheckTimeout)
	defer cancel()
	v, ok, err := h.profiles.PublicVersion(ctx, userID, freshAfter)
	if err != nil {
		return "", false, fmt.Errorf("checking public profile: %w", err)
	}
	return v, ok, nil
}

// minutesData is the owner stream's event payload.
type minutesData struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// ownerLoop sends every (coalesced) change to the user's own stream, with
// its time range. It returns when the stream ends for any reason.
func (h *Handler) ownerLoop(s *stream, sub *Subscription) {
	if !s.start() {
		return
	}
	defer s.stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-sub.Done():
			return
		case <-s.deadline.C:
			return
		case <-s.keepalive.C:
			if !s.write(": keepalive\n\n") {
				return
			}
		case <-sub.Notify():
			ev, ok := sub.Take()
			if !ok {
				continue
			}
			data, err := json.Marshal(minutesData(ev))
			if err != nil {
				slog.WarnContext(s.ctx, "dropping live event", "err", err)
				continue
			}
			if !s.write("event: minutes\ndata: " + string(data) + "\n\n") {
				return
			}
		}
	}
}

// publicLoop sends an empty event only when what the public profile shows
// has changed, checked at most once per PublicInterval. Changes to hidden
// projects or to agent and idle time never reach viewers, and the event
// carries no range, so viewers learn nothing the profile doesn't show.
func (h *Handler) publicLoop(s *stream, sub *Subscription, userID uuid.UUID, version string) {
	if !s.start() {
		return
	}
	defer s.stop()
	check := time.NewTimer(0)
	check.Stop()
	defer check.Stop()
	armed := false
	lastCheck := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-sub.Done():
			return
		case <-s.deadline.C:
			return
		case <-s.keepalive.C:
			if !s.write(": keepalive\n\n") {
				return
			}
		case <-sub.Notify():
			sub.Take() // the range is never shown to viewers
			if !armed {
				check.Reset(max(0, h.cfg.PublicInterval-time.Since(lastCheck)))
				armed = true
			}
		case <-check.C:
			armed = false
			lastCheck = time.Now()
			// freshAfter is after the notification, so the version reflects
			// the change that triggered it.
			v, ok, err := h.version(s.ctx, userID, lastCheck)
			switch {
			case err != nil:
				slog.WarnContext(s.ctx, "live public check failed", "err", err)
			case !ok:
				return
			case v != version:
				version = v
				if !s.write("event: minutes\ndata: {}\n\n") {
					return
				}
			}
		}
	}
}

// stream is one open SSE response.
type stream struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	ctx context.Context
	cfg Config

	keepalive *time.Ticker
	deadline  *time.Timer
}

func newStream(w http.ResponseWriter, r *http.Request, cfg Config) *stream {
	return &stream{w: w, rc: http.NewResponseController(w), ctx: r.Context(), cfg: cfg}
}

// start writes the headers and the ready event and starts the timers. It
// reports false when the client is already gone.
func (s *stream) start() bool {
	hdr := s.w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no") // stop nginx-style proxies buffering the stream
	s.w.WriteHeader(http.StatusOK)
	if !s.write(fmt.Sprintf("retry: %d\nevent: ready\ndata: {}\n\n", s.cfg.Retry.Milliseconds())) {
		return false
	}
	s.keepalive = time.NewTicker(s.cfg.Keepalive)
	s.deadline = time.NewTimer(s.cfg.MaxDuration)
	return true
}

func (s *stream) stop() {
	s.keepalive.Stop()
	s.deadline.Stop()
}

// write sends one frame and flushes it, reporting false once the client
// can't be written to.
func (s *stream) write(frame string) bool {
	// Deadlines are best-effort: test recorders don't support them.
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return false
	}
	//nolint:gosec // text/event-stream of server-built frames and JSON, never rendered as HTML
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		return false
	}
	if err := s.rc.Flush(); err != nil {
		slog.DebugContext(s.ctx, "live stream flush failed", "err", err)
		return false
	}
	return true
}
