package hackatime

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// stateCookie carries the sealed OAuth state between /link and /callback.
const stateCookie = "logbook_hackatime_link"

// stateCookiePath scopes the cookie to the two auth endpoints.
const stateCookiePath = "/auth/hackatime"

// HandlerOptions are Handler settings.
type HandlerOptions struct {
	// InsecureCookies drops the Secure flag, for local http development only.
	InsecureCookies bool
	// SettingsPath is where the browser lands after linking. Default "/settings".
	SettingsPath string
}

// Handler serves the Hackatime link endpoints.
type Handler struct {
	svc            *Service
	requireSession httpapi.Middleware
	secure         bool
	settings       string
}

// NewHandler builds the handler. requireSession must authenticate a browser
// session and enforce CSRF on state-changing methods.
func NewHandler(svc *Service, requireSession httpapi.Middleware, opts HandlerOptions) *Handler {
	h := &Handler{svc: svc, requireSession: requireSession, secure: !opts.InsecureCookies, settings: opts.SettingsPath}
	if h.settings == "" {
		h.settings = "/settings"
	}
	return h
}

// Register adds the routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /auth/hackatime/link", h.requireSession(httpapi.Handle(h.link)))
	mux.Handle("GET /auth/hackatime/callback", h.requireSession(httpapi.Handle(h.callback)))
	mux.Handle("GET /api/v1/hackatime/link", h.requireSession(httpapi.Handle(h.status)))
	mux.Handle("DELETE /api/v1/hackatime/link", h.requireSession(httpapi.Handle(h.unlink)))
}

func currentUser(r *http.Request) (httpapi.CurrentUser, error) {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return httpapi.CurrentUser{}, httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	return u, nil
}

func (h *Handler) link(w http.ResponseWriter, r *http.Request) error {
	u, err := currentUser(r)
	if err != nil {
		return err
	}
	authURL, cookie, err := h.svc.BeginLink(u.ID)
	if errors.Is(err, ErrNotConfigured) {
		return httpapi.Errorf(http.StatusServiceUnavailable, err, "Hackatime linking is not configured on this server")
	}
	if err != nil {
		return err
	}
	h.setStateCookie(w, cookie, int(stateTTL/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authURL, http.StatusFound)
	return nil
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) error {
	u, err := currentUser(r)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	h.setStateCookie(w, "", -1) // state is single use, whatever happens next
	q := r.URL.Query()
	if q.Get("error") != "" {
		// The user declined on Hackatime's consent screen.
		http.Redirect(w, r, h.settingsURL("denied"), http.StatusSeeOther)
		return nil
	}
	var cookie string
	if c, err := r.Cookie(stateCookie); err == nil {
		cookie = c.Value
	}
	err = h.svc.CompleteLink(r.Context(), u.ID, cookie, q.Get("state"), q.Get("code"))
	switch {
	case err == nil:
		http.Redirect(w, r, h.settingsURL("linked"), http.StatusSeeOther)
		return nil
	case errors.Is(err, ErrBadState):
		return httpapi.Errorf(http.StatusBadRequest, err, "invalid or expired link request; start linking again")
	case errors.Is(err, ErrAuthorizationRejected):
		return httpapi.Errorf(http.StatusBadRequest, err, "Hackatime did not accept the authorization; start linking again")
	case errors.Is(err, ErrAlreadyLinked):
		return httpapi.Errorf(http.StatusConflict, err, "this Hackatime account is already linked to another Logbook account")
	case errors.Is(err, ErrNotConfigured):
		return httpapi.Errorf(http.StatusServiceUnavailable, err, "Hackatime linking is not configured on this server")
	case isUpstream(err):
		return httpapi.Errorf(http.StatusBadGateway, err, "Hackatime is unavailable; try again later")
	default:
		return err
	}
}

func isUpstream(err error) bool {
	var se *StatusError
	var rl *RateLimitedError
	return errors.As(err, &se) || errors.As(err, &rl) || errors.Is(err, ErrInvalidResponse) ||
		errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrResponseTooLarge) || errors.Is(err, ErrNotFound)
}

func (h *Handler) settingsURL(result string) string {
	return h.settings + "?" + url.Values{"hackatime": {result}}.Encode()
}

func (h *Handler) setStateCookie(w http.ResponseWriter, value string, maxAge int) {
	// Secure is off only when InsecureCookies is set for local http development.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above
		Name:     stateCookie,
		Value:    value,
		Path:     stateCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   h.secure,
		// Lax: the cookie must ride along on Hackatime's top-level redirect back.
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) error {
	u, err := currentUser(r)
	if err != nil {
		return err
	}
	st, err := h.svc.Status(r.Context(), u.ID)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusOK, toStatusJSON(st))
	return nil
}

func (h *Handler) unlink(w http.ResponseWriter, r *http.Request) error {
	u, err := currentUser(r)
	if err != nil {
		return err
	}
	if err := h.svc.Unlink(r.Context(), u.ID); errors.Is(err, ErrNotLinked) {
		return httpapi.Errorf(http.StatusNotFound, err, "no Hackatime account is linked")
	} else if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// StatusJSON is the body of GET /api/v1/hackatime/link. It never includes
// the token, and shows the trust level as Hackatime shows it to the user.
type StatusJSON struct {
	Linked            bool        `json:"linked"`
	HackatimeUserID   int64       `json:"hackatime_user_id,omitempty"`
	HackatimeUsername string      `json:"hackatime_username,omitempty"`
	TrustLevel        TrustLevel  `json:"trust_level,omitempty"`
	TrustCheckedAt    *time.Time  `json:"trust_checked_at,omitempty"`
	LinkedAt          *time.Time  `json:"linked_at,omitempty"`
	Import            *ImportJSON `json:"import,omitempty"`
}

// ImportJSON is the history import's progress.
type ImportJSON struct {
	Status             string    `json:"status"`
	FromDate           string    `json:"from_date"`
	ToDate             string    `json:"to_date"`
	NextDate           string    `json:"next_date"`
	DaysTotal          int       `json:"days_total"`
	DaysDone           int       `json:"days_done"`
	HeartbeatsImported int64     `json:"heartbeats_imported"`
	LastError          string    `json:"last_error"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func toStatusJSON(st Status) StatusJSON {
	if !st.Linked {
		return StatusJSON{Linked: false}
	}
	l := st.Link
	linkedAt := l.LinkedAt
	out := StatusJSON{
		Linked:            true,
		HackatimeUserID:   l.HackatimeUserID,
		HackatimeUsername: l.HackatimeUsername,
		TrustLevel:        TrustLevel(l.TrustLevel).Public(),
		TrustCheckedAt:    l.TrustCheckedAt,
		LinkedAt:          &linkedAt,
	}
	if st.Import != nil {
		out.Import = toImportJSON(*st.Import)
	}
	return out
}

func toImportJSON(imp db.HackatimeImport) *ImportJSON {
	from, to, next := imp.FromDate.Time, imp.ToDate.Time, imp.NextDate.Time
	total := int(to.Sub(from).Hours()/24) + 1
	done := min(max(int(next.Sub(from).Hours()/24), 0), total)
	return &ImportJSON{
		Status:             imp.Status,
		FromDate:           from.Format(time.DateOnly),
		ToDate:             to.Format(time.DateOnly),
		NextDate:           next.Format(time.DateOnly),
		DaysTotal:          total,
		DaysDone:           done,
		HeartbeatsImported: imp.HeartbeatsImported,
		LastError:          imp.LastError,
		UpdatedAt:          imp.UpdatedAt,
	}
}
