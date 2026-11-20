package profiles

import (
	"errors"
	"net/http"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// Handler serves public profiles and the signed-in user's profile settings.
type Handler struct {
	svc            *Service
	requireSession httpapi.Middleware
}

// NewHandler returns a Handler; requireSession must authenticate the caller
// and enforce CSRF on PATCH. The public profile route needs no auth.
func NewHandler(svc *Service, requireSession httpapi.Middleware) *Handler {
	return &Handler{svc: svc, requireSession: requireSession}
}

// Register mounts the profile routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/users/{username}/profile", httpapi.Handle(h.public))
	mux.Handle("GET /api/v1/me/profile", h.requireSession(httpapi.Handle(h.me)))
	mux.Handle("PATCH /api/v1/me/profile", h.requireSession(httpapi.Handle(h.update)))
}

// patchRequest is the PATCH body; absent fields are left unchanged.
type patchRequest struct {
	DisplayName   *string `json:"display_name"`
	Timezone      *string `json:"timezone"`
	ProfilePublic *bool   `json:"profile_public"`
}

func (h *Handler) public(w http.ResponseWriter, r *http.Request) error {
	p, err := h.svc.Public(r.Context(), r.PathValue("username"))
	if err != nil {
		return mapError(err)
	}
	// Revalidate every time so making a profile private takes effect at once.
	w.Header().Set("Cache-Control", "no-cache")
	httpapi.WriteJSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	m, err := h.svc.Me(r.Context(), u.ID)
	if err != nil {
		return mapError(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusOK, m)
	return nil
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	var req patchRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	m, err := h.svc.Update(r.Context(), u.ID, Patch(req))
	if err != nil {
		return mapError(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusOK, m)
	return nil
}

func mapError(err error) error {
	for _, e := range []error{ErrInvalidDisplayName, ErrInvalidTimezone, ErrEmptyPatch} {
		if errors.Is(err, e) {
			return httpapi.Errorf(http.StatusBadRequest, err, "%s", e.Error())
		}
	}
	if errors.Is(err, ErrNotFound) {
		return httpapi.Errorf(http.StatusNotFound, err, "not found")
	}
	return err
}
