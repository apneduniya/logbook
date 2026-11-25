package oauth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// Rate limits (programs.md): /oauth/token and /oauth/revoke share a budget
// per client of tokenBurst, refilled per minute. /oauth/authorize is
// limited per IP, since each call may store a pending request.
const (
	tokenBurst     = 60
	authorizeBurst = 120
	limiterMaxKeys = 10_000
	limiterIdleTTL = 10 * time.Minute
)

// HandlerOptions configures a Handler.
type HandlerOptions struct {
	// TrustProxy keys IP rate limits on the proxy-set X-Forwarded-For hop.
	TrustProxy bool
}

// Handler serves the OAuth, app, grant and program routes.
type Handler struct {
	svc             *Service
	requireSession  httpapi.Middleware
	optionalSession httpapi.Middleware
	publicLimit     httpapi.Middleware
	authorizeLimit  httpapi.Middleware
	clientLimiter   *httpapi.RateLimiter
	trustProxy      bool
}

// NewHandler returns a Handler. requireSession must authenticate a browser
// session and enforce CSRF on writes; optionalSession attaches a session
// user when there is one (GET /oauth/authorize). Both must put an
// httpapi.CurrentUser on the context and must not accept bearer tokens or
// API keys. publicLimit rate-limits the public program list.
func NewHandler(svc *Service, requireSession, optionalSession, publicLimit httpapi.Middleware, opts HandlerOptions) *Handler {
	perMinute := func(burst int) *httpapi.RateLimiter {
		return httpapi.NewRateLimiter(float64(burst)/60, burst, limiterMaxKeys, limiterIdleTTL)
	}
	return &Handler{
		svc: svc, requireSession: requireSession, optionalSession: optionalSession, publicLimit: publicLimit,
		authorizeLimit: perMinute(authorizeBurst).Middleware(httpapi.IPKey(opts.TrustProxy)),
		clientLimiter:  perMinute(tokenBurst),
		trustProxy:     opts.TrustProxy,
	}
}

// Register mounts the routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	sess := func(f httpapi.HandlerFunc) http.Handler { return h.requireSession(httpapi.Handle(f)) }

	mux.Handle("GET /api/v1/me/apps", sess(h.listApps))
	mux.Handle("POST /api/v1/me/apps", sess(h.createApp))
	mux.Handle("GET /api/v1/me/apps/{id}", sess(h.getApp))
	mux.Handle("PATCH /api/v1/me/apps/{id}", sess(h.updateApp))
	mux.Handle("DELETE /api/v1/me/apps/{id}", sess(h.deleteApp))
	mux.Handle("POST /api/v1/me/apps/{id}/secret", sess(h.rotateSecret))
	mux.Handle("GET /api/v1/me/apps/{id}/members", sess(h.listMembers))
	mux.Handle("POST /api/v1/me/apps/{id}/members", sess(h.addMember))
	mux.Handle("DELETE /api/v1/me/apps/{id}/members/{username}", sess(h.removeMember))
	mux.Handle("GET /api/v1/programs", h.publicLimit(httpapi.Handle(h.programs)))

	mux.Handle("GET /api/v1/me/grants", sess(h.listGrants))
	mux.Handle("DELETE /api/v1/me/grants/{app_id}", sess(h.revokeGrant))

	mux.Handle("GET /oauth/authorize", httpapi.Chain(httpapi.Handle(h.authorize), h.authorizeLimit, h.optionalSession))
	mux.Handle("GET /api/v1/oauth/requests/{id}", sess(h.getRequest))
	mux.Handle("POST /api/v1/oauth/requests/{id}", sess(h.decideRequest))
	mux.Handle("POST /oauth/token", http.HandlerFunc(h.token))
	mux.Handle("POST /oauth/revoke", http.HandlerFunc(h.revoke))
}

func currentUser(r *http.Request) (uuid.UUID, error) {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return uuid.Nil, httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	}
	return u.ID, nil
}

// pathID parses a uuid path value; a malformed one is simply not found.
func pathID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, mapError(ErrNotFound)
	}
	return id, nil
}

func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

type createAppRequest struct {
	Name         string   `json:"name"`
	Slug         string   `json:"slug"`
	RedirectURIs []string `json:"redirect_uris"`
	HomepageURL  string   `json:"homepage_url"`
	Public       bool     `json:"public"`
	Confidential *bool    `json:"confidential"`
}

func (h *Handler) createApp(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	var req createAppRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	in := AppInput{
		Name: req.Name, Slug: req.Slug, RedirectURIs: req.RedirectURIs, HomepageURL: req.HomepageURL,
		Public: req.Public, Confidential: req.Confidential == nil || *req.Confidential,
	}
	app, err := h.svc.CreateApp(r.Context(), user, in)
	if err != nil {
		return mapError(err)
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusCreated, app)
	return nil
}

func (h *Handler) listApps(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	apps, err := h.svc.ListApps(r.Context(), user)
	if err != nil {
		return err
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusOK, map[string][]App{"apps": apps})
	return nil
}

func (h *Handler) getApp(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	app, err := h.svc.GetApp(r.Context(), user, id)
	if err != nil {
		return mapError(err)
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusOK, app)
	return nil
}

type updateAppRequest struct {
	Name         *string   `json:"name"`
	Slug         *string   `json:"slug"`
	RedirectURIs *[]string `json:"redirect_uris"`
	HomepageURL  *string   `json:"homepage_url"`
	Public       *bool     `json:"public"`
	Confidential *bool     `json:"confidential"`
}

func (h *Handler) updateApp(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req updateAppRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.Slug != nil || req.Confidential != nil {
		return httpapi.Errorf(http.StatusBadRequest, nil, "slug and confidential can't be changed")
	}
	app, err := h.svc.UpdateApp(r.Context(), user, id, AppPatch{
		Name: req.Name, RedirectURIs: req.RedirectURIs, HomepageURL: req.HomepageURL, Public: req.Public,
	})
	if err != nil {
		return mapError(err)
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusOK, app)
	return nil
}

func (h *Handler) rotateSecret(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	app, err := h.svc.RotateSecret(r.Context(), user, id)
	if err != nil {
		return mapError(err)
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusOK, app)
	return nil
}

func (h *Handler) deleteApp(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := h.svc.DeleteApp(r.Context(), user, id); err != nil {
		return mapError(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ms, err := h.svc.Members(r.Context(), user, id)
	if err != nil {
		return mapError(err)
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusOK, map[string][]Member{"members": ms})
	return nil
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	m, err := h.svc.AddMember(r.Context(), user, id, req.Username)
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusCreated, m)
	return nil
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := h.svc.RemoveMember(r.Context(), user, id, r.PathValue("username")); err != nil {
		return mapError(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) programs(w http.ResponseWriter, r *http.Request) error {
	ps, err := h.svc.Programs(r.Context())
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	httpapi.WriteJSON(w, http.StatusOK, map[string][]Program{"programs": ps})
	return nil
}

func (h *Handler) listGrants(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	gs, err := h.svc.Grants(r.Context(), user)
	if err != nil {
		return err
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusOK, map[string][]Grant{"grants": gs})
	return nil
}

func (h *Handler) revokeGrant(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "app_id")
	if err != nil {
		return err
	}
	if err := h.svc.RevokeGrant(r.Context(), user, id); err != nil {
		return mapError(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	user := uuid.Nil
	if u, ok := httpapi.UserFrom(r.Context()); ok {
		user = u.ID
	}
	target, err := h.svc.Authorize(r.Context(), AuthorizeParams{
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), ResponseType: q.Get("response_type"),
		Scope: q.Get("scope"), State: q.Get("state"),
		CodeChallenge: q.Get("code_challenge"), CodeChallengeMethod: q.Get("code_challenge_method"),
	}, user)
	if err != nil {
		return err
	}
	noStore(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target, http.StatusFound)
	return nil
}

func (h *Handler) getRequest(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	cr, err := h.svc.ConsentRequest(r.Context(), user, r.PathValue("id"))
	if err != nil {
		return mapError(err)
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusOK, cr)
	return nil
}

func (h *Handler) decideRequest(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	var req struct {
		Approve *bool `json:"approve"`
	}
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.Approve == nil {
		return httpapi.Errorf(http.StatusBadRequest, nil, "approve must be true or false")
	}
	target, err := h.svc.Decide(r.Context(), user, r.PathValue("id"), *req.Approve)
	if err != nil {
		return mapError(err)
	}
	noStore(w)
	httpapi.WriteJSON(w, http.StatusOK, map[string]string{"redirect": target})
	return nil
}

// mapError turns service errors into client-safe HTTP errors.
func mapError(err error) error {
	switch {
	case errors.Is(err, ErrInvalid):
		return httpapi.Errorf(http.StatusBadRequest, err, "%s", strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	case errors.Is(err, ErrNotFound):
		return httpapi.Errorf(http.StatusNotFound, err, "not found")
	case errors.Is(err, ErrNotOwner):
		return httpapi.Errorf(http.StatusForbidden, err, "%s", ErrNotOwner.Error())
	case errors.Is(err, ErrSlugTaken):
		return httpapi.Errorf(http.StatusConflict, err, "%s", ErrSlugTaken.Error())
	case errors.Is(err, ErrTooManyApps):
		return httpapi.Errorf(http.StatusConflict, err, "you already have %d apps; delete one first", MaxAppsPerUser)
	case errors.Is(err, ErrAlreadyMember):
		return httpapi.Errorf(http.StatusConflict, err, "%s", ErrAlreadyMember.Error())
	case errors.Is(err, ErrTooManyReviewers):
		return httpapi.Errorf(http.StatusConflict, err, "an app can have at most %d reviewers", MaxReviewers)
	case errors.Is(err, ErrNoSecret):
		return httpapi.Errorf(http.StatusConflict, err, "%s", ErrNoSecret.Error())
	case errors.Is(err, ErrUserNotFound):
		return httpapi.Errorf(http.StatusNotFound, err, "%s", ErrUserNotFound.Error())
	}
	return err
}
