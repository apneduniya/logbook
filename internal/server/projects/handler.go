package projects

import (
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// MaxNameLen bounds project names in paths, matching the wire limit.
const MaxNameLen = 256

// Handler serves the signed-in user's projects.
type Handler struct {
	svc            *Service
	requireSession httpapi.Middleware
}

// NewHandler returns a Handler; requireSession must authenticate the caller
// and enforce CSRF on PATCH.
func NewHandler(svc *Service, requireSession httpapi.Middleware) *Handler {
	return &Handler{svc: svc, requireSession: requireSession}
}

// Register mounts the project routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/me/projects", h.requireSession(httpapi.Handle(h.list)))
	mux.Handle("GET /api/v1/me/projects/{name}", h.requireSession(httpapi.Handle(h.get)))
	mux.Handle("PATCH /api/v1/me/projects/{name}", h.requireSession(httpapi.Handle(h.update)))
}

// patchRequest is the PATCH body; absent fields are left unchanged.
type patchRequest struct {
	RepoURL  *string `json:"repo_url"`
	Hidden   *bool   `json:"hidden"`
	Archived *bool   `json:"archived"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	ps, err := h.svc.List(r.Context(), u.ID)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"projects": ps})
	return nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	name, err := pathName(r)
	if err != nil {
		return err
	}
	p, err := h.svc.Get(r.Context(), u.ID, name)
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	name, err := pathName(r)
	if err != nil {
		return err
	}
	var req patchRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	p, err := h.svc.Update(r.Context(), u.ID, name, Patch(req))
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, p)
	return nil
}

func pathName(r *http.Request) (string, error) {
	name := r.PathValue("name")
	if name == "" || len(name) > MaxNameLen || !utf8.ValidString(name) {
		return "", httpapi.Errorf(http.StatusBadRequest, nil, "invalid project name")
	}
	return name, nil
}

func mapError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return httpapi.Errorf(http.StatusNotFound, err, "project not found")
	case errors.Is(err, ErrInvalidRepoURL):
		return httpapi.Errorf(http.StatusBadRequest, err, "%s", ErrInvalidRepoURL.Error())
	case errors.Is(err, ErrEmptyPatch):
		return httpapi.Errorf(http.StatusBadRequest, err, "no fields to update")
	}
	return err
}
