package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Route prefixes. /api/hackatime/v1 lets users swap only the host in a
// Hackatime api_url; /api/v1 is the plain WakaTime layout.
const (
	wakatimePrefix  = "/api/v1"
	hackatimePrefix = "/api/hackatime/v1"
)

// SourceHeader marks a request as coming from the Logbook agent rather than
// straight from wakatime-cli. It is self-reported provenance, not a trust signal.
const SourceHeader = "X-Logbook-Source"

// retryAfterSeconds is the Retry-After value sent with 429 responses.
const retryAfterSeconds = "60"

// service is what the handler needs from Service.
type service interface {
	IngestBatch(ctx context.Context, userID uuid.UUID, hbs []wire.Heartbeat, source string, client Client) (BatchResult, error)
	StatusBarToday(ctx context.Context, userID uuid.UUID) (StatusBar, error)
}

// Handler serves the WakaTime-compatible heartbeat and status-bar endpoints.
type Handler struct {
	svc           service
	requireAPIKey httpapi.Middleware
	limiter       *limiter // heartbeat POSTs
	readLimiter   *limiter // status-bar GETs, a separate bucket
}

// HandlerOption configures a Handler.
type HandlerOption func(*handlerConfig)

type handlerConfig struct {
	rate, burst         float64
	readRate, readBurst float64
	now                 func() time.Time
}

// WithRateLimit sets the per-user request rate (per second) and burst for
// the heartbeat endpoints.
func WithRateLimit(perSecond, burst float64) HandlerOption {
	return func(c *handlerConfig) { c.rate, c.burst = perSecond, burst }
}

// WithStatusBarRateLimit sets the per-user request rate (per second) and
// burst for the status-bar endpoint.
func WithStatusBarRateLimit(perSecond, burst float64) HandlerOption {
	return func(c *handlerConfig) { c.readRate, c.readBurst = perSecond, burst }
}

// NewHandler returns a Handler. requireAPIKey must authenticate the caller
// and put an httpapi.CurrentUser on the context.
func NewHandler(svc *Service, requireAPIKey httpapi.Middleware, opts ...HandlerOption) *Handler {
	return newHandler(svc, requireAPIKey, opts...)
}

func newHandler(svc service, requireAPIKey httpapi.Middleware, opts ...HandlerOption) *Handler {
	c := handlerConfig{
		rate: defaultRatePerSecond, burst: defaultBurst,
		readRate: defaultStatusBarRatePerSecond, readBurst: defaultStatusBarBurst,
		now: time.Now,
	}
	for _, o := range opts {
		o(&c)
	}
	return &Handler{
		svc:           svc,
		requireAPIKey: requireAPIKey,
		limiter:       newLimiter(c.rate, c.burst, c.now),
		readLimiter:   newLimiter(c.readRate, c.readBurst, c.now),
	}
}

// Register adds the ingest routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	for _, p := range []struct {
		prefix       string
		singleStatus int
	}{
		// WakaTime answers a single heartbeat with 201, Hackatime with 202.
		{wakatimePrefix, http.StatusCreated},
		{hackatimePrefix, http.StatusAccepted},
	} {
		mux.Handle("POST "+p.prefix+"/users/current/heartbeats", h.wrap(h.single(p.singleStatus)))
		mux.Handle("POST "+p.prefix+"/users/current/heartbeats.bulk", h.wrap(h.bulk))
		mux.Handle("GET "+p.prefix+"/users/current/statusbar/today", h.wrap(h.statusBar))
	}
}

func (h *Handler) wrap(f httpapi.HandlerFunc) http.Handler {
	return httpapi.Chain(httpapi.Handle(f), h.requireAPIKey)
}

// single handles one heartbeat, sent as an object or a one-element array.
func (h *Handler) single(okStatus int) httpapi.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		user, err := h.caller(w, r)
		if err != nil {
			return err
		}
		items, err := decodeItems(w, r)
		if err != nil {
			return err
		}
		if len(items) > 1 {
			return httpapi.Errorf(http.StatusBadRequest, nil, "send more than one heartbeat to heartbeats.bulk")
		}
		hb, err := decodeHeartbeat(items[0])
		if err != nil {
			return httpapi.Errorf(http.StatusBadRequest, err, "%s", err.Error())
		}
		res, err := h.svc.IngestBatch(r.Context(), user.ID, []wire.Heartbeat{hb}, sourceOf(r), clientOf(r))
		if err != nil {
			return fmt.Errorf("ingesting heartbeat: %w", err)
		}
		item := res.Items[0]
		if item.Err != nil {
			return httpapi.Errorf(http.StatusBadRequest, item.Err, "%s", item.Err.Error())
		}
		httpapi.WriteJSON(w, okStatus, map[string]heartbeatData{"data": newHeartbeatData(item)})
		return nil
	}
}

// bulk handles up to wire.MaxBulkHeartbeats heartbeats and answers with one
// [body, status] pair per heartbeat, in order, as wakatime-cli expects.
func (h *Handler) bulk(w http.ResponseWriter, r *http.Request) error {
	user, err := h.caller(w, r)
	if err != nil {
		return err
	}
	items, err := decodeItems(w, r)
	if err != nil {
		return err
	}
	if len(items) > wire.MaxBulkHeartbeats {
		return httpapi.Errorf(http.StatusBadRequest, nil,
			"Too many heartbeats in a single request (max %d)", wire.MaxBulkHeartbeats)
	}

	// Items that aren't even heartbeat-shaped get their 400 here; the rest go
	// to the service, which validates them.
	decodeErrs := make([]error, len(items))
	hbs := make([]wire.Heartbeat, 0, len(items))
	for i, raw := range items {
		hb, err := decodeHeartbeat(raw)
		if err != nil {
			decodeErrs[i] = err
			continue
		}
		hbs = append(hbs, hb)
	}
	res, err := h.svc.IngestBatch(r.Context(), user.ID, hbs, sourceOf(r), clientOf(r))
	if err != nil {
		return fmt.Errorf("ingesting heartbeats: %w", err)
	}

	responses := make([][2]any, len(items))
	next := 0
	for i := range items {
		if decodeErrs[i] != nil {
			responses[i] = [2]any{errorBody{decodeErrs[i].Error()}, http.StatusBadRequest}
			continue
		}
		item := res.Items[next]
		next++
		if item.Err != nil {
			// 400 tells wakatime-cli to drop the heartbeat instead of retrying it.
			responses[i] = [2]any{errorBody{item.Err.Error()}, http.StatusBadRequest}
			continue
		}
		responses[i] = [2]any{map[string]heartbeatData{"data": newHeartbeatData(item)}, http.StatusCreated}
	}
	httpapi.WriteJSON(w, http.StatusCreated, map[string]any{"responses": responses})
	return nil
}

func (h *Handler) statusBar(w http.ResponseWriter, r *http.Request) error {
	user, err := callerLimited(w, r, h.readLimiter)
	if err != nil {
		return err
	}
	sb, err := h.svc.StatusBarToday(r.Context(), user.ID)
	if err != nil {
		if store.IsNotFound(err) {
			return httpapi.Errorf(http.StatusUnauthorized, err, "unauthorized")
		}
		return fmt.Errorf("building status bar: %w", err)
	}
	httpapi.WriteJSON(w, http.StatusOK, sb)
	return nil
}

// caller returns the authenticated user and applies the per-user heartbeat
// rate limit. wakatime-cli re-queues heartbeats from a rejected request and
// retries later.
func (h *Handler) caller(w http.ResponseWriter, r *http.Request) (httpapi.CurrentUser, error) {
	return callerLimited(w, r, h.limiter)
}

// callerLimited returns the authenticated user after taking a token from
// their bucket in l, or a 429 with Retry-After when the bucket is empty.
func callerLimited(w http.ResponseWriter, r *http.Request, l *limiter) (httpapi.CurrentUser, error) {
	user, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.CurrentUser{}, httpapi.Errorf(http.StatusUnauthorized, nil, "unauthorized")
	}
	if !l.allow(user.ID) {
		w.Header().Set("Retry-After", retryAfterSeconds)
		return httpapi.CurrentUser{}, httpapi.Errorf(http.StatusTooManyRequests, nil, "Too many requests, try again later")
	}
	return user, nil
}

// decodeItems reads the body as a heartbeat object or an array of them. A
// whole-request error is returned as an *httpapi.Error.
func decodeItems(w http.ResponseWriter, r *http.Request) ([]json.RawMessage, error) {
	var raw json.RawMessage
	if err := httpapi.DecodeJSON(w, r, &raw); err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	var items []json.RawMessage
	switch {
	case len(raw) > 0 && raw[0] == '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, httpapi.Errorf(http.StatusBadRequest, err, "invalid JSON body")
		}
	case len(raw) > 0 && raw[0] == '{':
		items = []json.RawMessage{raw}
	default:
		return nil, httpapi.Errorf(http.StatusBadRequest, nil, "body must be a heartbeat object or an array of heartbeats")
	}
	if len(items) == 0 {
		return nil, httpapi.Errorf(http.StatusBadRequest, nil, "No data provided")
	}
	return items, nil
}

func decodeHeartbeat(raw json.RawMessage) (wire.Heartbeat, error) {
	var hb wire.Heartbeat
	if err := json.Unmarshal(raw, &hb); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field != "" {
			return wire.Heartbeat{}, fmt.Errorf("%w: %s has the wrong type", wire.ErrInvalidHeartbeat, ute.Field)
		}
		return wire.Heartbeat{}, fmt.Errorf("%w: not a heartbeat object", wire.ErrInvalidHeartbeat)
	}
	return hb, nil
}

// clientOf reads the values wakatime-cli sends in headers. How they combine
// with each heartbeat's body is the service's rule (see Client).
func clientOf(r *http.Request) Client {
	m := r.Header.Get("X-Machine-Name")
	// wakatime-cli query-escapes the hostname.
	if un, err := url.QueryUnescape(m); err == nil {
		m = un
	}
	return Client{UserAgent: r.UserAgent(), Machine: m}
}

func sourceOf(r *http.Request) string {
	if r.Header.Get(SourceHeader) == SourceAgent {
		return SourceAgent
	}
	return SourceEditor
}

type errorBody struct {
	Error string `json:"error"`
}

// heartbeatData is the stored heartbeat as echoed back to clients.
type heartbeatData struct {
	ID              string  `json:"id"`
	Entity          string  `json:"entity"`
	Type            string  `json:"type"`
	Category        string  `json:"category"`
	Time            float64 `json:"time"`
	Project         string  `json:"project"`
	Branch          string  `json:"branch"`
	Language        string  `json:"language"`
	Editor          string  `json:"editor"`
	Machine         string  `json:"machine"`
	OperatingSystem string  `json:"operating_system"`
	UserAgent       string  `json:"user_agent"`
	IsWrite         bool    `json:"is_write"`
}

func newHeartbeatData(it ItemResult) heartbeatData {
	h := it.Heartbeat
	return heartbeatData{
		ID: it.ID, Entity: h.Entity, Type: h.Type, Category: h.Category, Time: h.Time,
		Project: h.Project, Branch: h.Branch, Language: h.Language, Editor: h.Editor,
		Machine: h.Machine, OperatingSystem: h.OperatingSystem, UserAgent: h.UserAgent, IsWrite: h.IsWrite,
	}
}
