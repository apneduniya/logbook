package stats

import (
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// MaxProjectNameLen bounds the project filter, matching the wire limit.
const MaxProjectNameLen = 256

// defaultRangeDays is the summary length when no start is given.
const defaultRangeDays = 7

// Handler serves the signed-in user's stats.
type Handler struct {
	svc            *Service
	requireSession httpapi.Middleware
}

// NewHandler returns a Handler; requireSession must authenticate the caller.
func NewHandler(svc *Service, requireSession httpapi.Middleware) *Handler {
	return &Handler{svc: svc, requireSession: requireSession}
}

// Register mounts the stats routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/me/summary", h.requireSession(httpapi.Handle(h.summary)))
	mux.Handle("GET /api/v1/me/today", h.requireSession(httpapi.Handle(h.today)))
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	tz, err := h.svc.Location(r.Context(), u.ID)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	end := DateOf(h.svc.Now(), tz)
	if s := q.Get("end"); s != "" {
		if end, err = ParseDate(s); err != nil {
			return httpapi.Errorf(http.StatusBadRequest, err, "end must be a date like 2026-11-20")
		}
	}
	start := end.AddDays(-(defaultRangeDays - 1))
	if s := q.Get("start"); s != "" {
		if start, err = ParseDate(s); err != nil {
			return httpapi.Errorf(http.StatusBadRequest, err, "start must be a date like 2026-11-20")
		}
	}
	if end.Before(start) {
		return httpapi.Errorf(http.StatusBadRequest, nil, "start must not be after end")
	}
	var f Filter
	if q.Has("project") {
		p := q.Get("project")
		if len(p) > MaxProjectNameLen || !utf8.ValidString(p) {
			return httpapi.Errorf(http.StatusBadRequest, nil, "project is invalid")
		}
		f.Project = &p
	}
	sum, err := h.svc.Summary(r.Context(), u.ID, start.Start(tz), end.AddDays(1).Start(tz), tz, f)
	if errors.Is(err, ErrInvalidRange) {
		return httpapi.Errorf(http.StatusBadRequest, err, "range must cover at most %d days", MaxRangeDays)
	}
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, sum)
	return nil
}

func (h *Handler) today(w http.ResponseWriter, r *http.Request) error {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	t, err := h.svc.Today(r.Context(), u.ID)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, t)
	return nil
}
