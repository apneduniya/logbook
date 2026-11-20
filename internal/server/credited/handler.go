package credited

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// Per-user upload limits, the same budget as heartbeat ingest: the agent
// uploads about once a minute and drains a backlog in batches of up to
// MaxBatch after being offline.
const (
	uploadRatePerSecond = 2
	uploadBurst         = 120
	limiterMaxUsers     = 10_000
	limiterIdleTTL      = 10 * time.Minute
)

// Handler serves POST /api/v1/devices/{id}/minutes.
type Handler struct {
	svc           *Service
	requireAPIKey httpapi.Middleware
	limit         httpapi.Middleware
}

// NewHandler returns a Handler. requireAPIKey must authenticate the caller
// and put an httpapi.CurrentUser on the context.
func NewHandler(svc *Service, requireAPIKey httpapi.Middleware) *Handler {
	l := httpapi.NewRateLimiter(uploadRatePerSecond, uploadBurst, limiterMaxUsers, limiterIdleTTL)
	return &Handler{svc: svc, requireAPIKey: requireAPIKey, limit: l.Middleware(httpapi.UserKey)}
}

// Register mounts the route on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/devices/{id}/minutes", httpapi.Chain(httpapi.Handle(h.upload), h.requireAPIKey, h.limit))
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "unauthorized")
	}
	deviceID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return mapError(ErrDeviceNotFound)
	}
	var b Batch
	if err := httpapi.DecodeJSON(w, r, &b); err != nil {
		return err
	}
	res, err := h.svc.Upload(r.Context(), u.ID, deviceID, b)
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, res)
	return nil
}

func mapError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidBatch):
		return httpapi.Errorf(http.StatusBadRequest, err, "%s", strings.TrimPrefix(err.Error(), ErrInvalidBatch.Error()+": "))
	case errors.Is(err, ErrDeviceNotFound):
		return httpapi.Errorf(http.StatusNotFound, err, "device not found")
	case errors.Is(err, ErrDeviceRevoked):
		return httpapi.Errorf(http.StatusForbidden, err, "device has been revoked")
	}
	return err
}
