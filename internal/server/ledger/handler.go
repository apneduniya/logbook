package ledger

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// Per-user upload limit: one record per device per day, plus retries and
// catching up after time offline.
const (
	uploadBurst     = 60
	limiterMaxUsers = 10_000
	limiterIdleTTL  = time.Hour
)

// Handler serves POST /api/v1/devices/{id}/evidence-days.
type Handler struct {
	svc           *Service
	requireAPIKey httpapi.Middleware
	limit         httpapi.Middleware
}

// NewHandler returns a Handler. requireAPIKey must authenticate the caller
// and put an httpapi.CurrentUser on the context.
func NewHandler(svc *Service, requireAPIKey httpapi.Middleware) *Handler {
	l := httpapi.NewRateLimiter(uploadBurst/60.0, uploadBurst, limiterMaxUsers, limiterIdleTTL)
	return &Handler{svc: svc, requireAPIKey: requireAPIKey, limit: l.Middleware(httpapi.UserKey)}
}

// Register mounts the route on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/devices/{id}/evidence-days", httpapi.Chain(httpapi.Handle(h.put), h.requireAPIKey, h.limit))
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "unauthorized")
	}
	deviceID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return mapError(ErrDeviceNotFound)
	}
	var up Upload
	if err := httpapi.DecodeJSON(w, r, &up); err != nil {
		return err
	}
	created, err := h.svc.Put(r.Context(), u.ID, deviceID, up)
	if err != nil {
		return mapError(err)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.WriteJSON(w, status, map[string]string{"day": up.Day})
	return nil
}

func mapError(err error) error {
	switch {
	case errors.Is(err, ErrInvalid):
		return httpapi.Errorf(http.StatusBadRequest, err, "%s", strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	case errors.Is(err, ErrConflict):
		return httpapi.Errorf(http.StatusConflict, err, "%s", ErrConflict.Error())
	case errors.Is(err, ErrChainMismatch):
		return httpapi.Errorf(http.StatusConflict, err, "%s", ErrChainMismatch.Error())
	case errors.Is(err, ErrDeviceNotFound):
		return httpapi.Errorf(http.StatusNotFound, err, "device not found")
	case errors.Is(err, ErrDeviceRevoked):
		return httpapi.Errorf(http.StatusForbidden, err, "device has been revoked")
	}
	return err
}
