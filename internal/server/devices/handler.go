package devices

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// Per-user registration limit: a burst of registerBurst, refilled at
// registerBurst per hour. An agent registers once per install or login; the
// burst is above MaxActiveDevices so the cap, not the limiter, answers first.
const (
	registerBurst   = 30
	limiterMaxUsers = 10_000
	limiterIdleTTL  = time.Hour
)

// Handler serves the device routes.
type Handler struct {
	svc            *Service
	requireAPIKey  httpapi.Middleware
	requireSession httpapi.Middleware
	registerLimit  httpapi.Middleware
}

// NewHandler returns a Handler. requireAPIKey and requireSession must
// authenticate the caller and put an httpapi.CurrentUser on the context;
// requireSession must also enforce CSRF on DELETE.
func NewHandler(svc *Service, requireAPIKey, requireSession httpapi.Middleware) *Handler {
	l := httpapi.NewRateLimiter(registerBurst/3600.0, registerBurst, limiterMaxUsers, limiterIdleTTL)
	return &Handler{
		svc: svc, requireAPIKey: requireAPIKey, requireSession: requireSession,
		registerLimit: l.Middleware(httpapi.UserKey),
	}
}

// Register mounts the device routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/devices", httpapi.Chain(httpapi.Handle(h.register), h.requireAPIKey, h.registerLimit))
	mux.Handle("GET /api/v1/me/devices", h.requireSession(httpapi.Handle(h.list)))
	mux.Handle("DELETE /api/v1/me/devices/{id}", h.requireSession(httpapi.Handle(h.revoke)))
}

type registerRequest struct {
	Name      string `json:"name"`
	OS        string `json:"os"`
	PublicKey string `json:"public_key"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "unauthorized")
	}
	var req registerRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	reg, err := ParseRegistration(req.Name, req.OS, req.PublicKey)
	if err != nil {
		return mapError(err)
	}
	out, err := h.svc.Register(r.Context(), u.ID, reg)
	if err != nil {
		return mapError(err)
	}
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	httpapi.WriteJSON(w, status, map[string]uuid.UUID{"id": out.ID})
	return nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	ds, err := h.svc.List(r.Context(), u.ID)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusOK, map[string][]Device{"devices": ds})
	return nil
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return mapError(ErrNotFound)
	}
	if err := h.svc.Revoke(r.Context(), u.ID, id); err != nil {
		return mapError(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func mapError(err error) error {
	switch {
	case errors.Is(err, ErrInvalid):
		return httpapi.Errorf(http.StatusBadRequest, err, "%s", strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	case errors.Is(err, ErrKeyTaken):
		return httpapi.Errorf(http.StatusConflict, err, "%s", ErrKeyTaken.Error())
	case errors.Is(err, ErrTooMany):
		return httpapi.Errorf(http.StatusConflict, err, "you already have %d active devices; revoke one first", MaxActiveDevices)
	case errors.Is(err, ErrNotFound):
		return httpapi.Errorf(http.StatusNotFound, err, "device not found")
	}
	return err
}
