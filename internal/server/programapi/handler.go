package programapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// Rate limit (ADR 0021): per token, or per client IP for calls without a
// valid credential, a burst of 600 refilled at 600 a minute.
const (
	RateBurst     = 600
	RatePerMinute = 600
)

// NewRateLimiter returns the program API's limiter.
func NewRateLimiter() *httpapi.RateLimiter {
	return httpapi.NewRateLimiter(RatePerMinute/60.0, RateBurst, 100_000, 10*time.Minute)
}

// Hackatime's error messages (stats_controller.rb, render_helpers.rb).
const (
	msgUserNotFound   = "User not found"
	msgPrivateStats   = "user has disabled public stats"
	msgUnauthorized   = "Unauthorized"
	msgProjectMissing = "found nuthin"
)

// closedRangeCache is the Cache-Control for responses about a range that
// has ended (ADR 0021).
const closedRangeCache = "private, max-age=300"

// Options configures a Handler.
type Options struct {
	// Limiter is shared by every route; nil means NewRateLimiter().
	Limiter *httpapi.RateLimiter
	// ClientKey turns a request into its client's rate-limit key (see
	// httpapi.IPKey); required.
	ClientKey func(*http.Request) string
}

// Handler serves the program API.
type Handler struct {
	svc       *Service
	auth      Authenticator
	limiter   *httpapi.RateLimiter
	clientKey func(*http.Request) string
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, auth Authenticator, opts Options) *Handler {
	if opts.Limiter == nil {
		opts.Limiter = NewRateLimiter()
	}
	return &Handler{svc: svc, auth: auth, limiter: opts.Limiter, clientKey: opts.ClientKey}
}

// Register mounts the routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/authenticated/me", h.oauth(ScopeProfile, true, h.me))
	mux.Handle("GET /api/v1/authenticated/projects", h.oauth(ScopeRead, false, h.authProjects))
	mux.Handle("GET /api/v1/users/{id}/stats", h.userRead(h.stats))
	mux.Handle("GET /api/v1/users/{id}/heartbeats/spans", h.userRead(h.spans))
	mux.Handle("GET /api/v1/users/{id}/projects", h.userRead(h.userProjects))
	mux.Handle("GET /api/v1/users/{id}/project/{name}", h.userRead(h.userProject))
	mux.Handle("GET /api/v1/users/{id}/projects/details", h.userRead(h.userProjectsDetails))
	mux.Handle("GET /api/v1/users/{id}/trust_factor", h.public(h.trustFactor))
	mux.Handle("GET /api/v1/my/heartbeats", h.keyOrToken(h.myHeartbeats))
}

// allow takes a token from the bucket for cred (when it verified) or for the
// client, writing a 429 and returning false when there is none.
func (h *Handler) allow(w http.ResponseWriter, r *http.Request, cred credential) bool {
	key := "ip:" + h.clientKey(r)
	if !cred.anonymous() {
		sum := sha256.Sum256([]byte(cred.secret))
		key = "cred:" + hex.EncodeToString(sum[:16])
	}
	if h.limiter.Allow(key) {
		return true
	}
	httpapi.WriteTooManyRequests(w, r, h.limiter.RetryAfter(key))
	return false
}

// banned reports whether user id is red, which Hackatime treats as having
// no API access (api_access_restricted?).
func (h *Handler) banned(r *http.Request, cred credential) (bool, error) {
	link, err := h.svc.Link(r.Context(), cred.userID)
	if err != nil {
		return false, err
	}
	return link.Trust.Banned(), nil
}

// oauth guards the authenticated/* routes as Doorkeeper does: a Bearer
// access token is required (401 invalid_token otherwise) and must carry
// scope (403 insufficient_scope otherwise), both with an empty body and a
// WWW-Authenticate header. Unless allowBanned, a red user then gets 401
// {"error": "Unauthorized"} (ensure_api_access_allowed). API keys are not
// accepted here, as on Hackatime.
func (h *Handler) oauth(scope string, allowBanned bool, f func(http.ResponseWriter, *http.Request, User) error) http.Handler {
	return httpapi.Handle(func(w http.ResponseWriter, r *http.Request) error {
		var cred credential
		var grant Grant
		if token, ok := bearerToken(r); ok {
			g, err := h.auth.AccessToken(r.Context(), token)
			switch {
			case err == nil:
				grant, cred = g, credential{userID: g.UserID, grant: &g, secret: token}
			case !errors.Is(err, ErrInvalidCredentials):
				return fmt.Errorf("checking access token: %w", err)
			}
		}
		if !h.allow(w, r, cred) {
			return nil
		}
		if cred.anonymous() {
			writeDoorkeeperError(w, http.StatusUnauthorized, "invalid_token", "The access token is invalid")
			return nil
		}
		if !grant.Has(scope) {
			writeDoorkeeperError(w, http.StatusForbidden, "insufficient_scope",
				fmt.Sprintf("Access to this resource requires scope %q.", scope))
			return nil
		}
		u, err := h.svc.UserByID(r.Context(), grant.UserID)
		if errors.Is(err, ErrUserNotFound) {
			writeDoorkeeperError(w, http.StatusUnauthorized, "invalid_token", "The access token is invalid")
			return nil
		}
		if err != nil {
			return err
		}
		if !allowBanned {
			if red, err := h.banned(r, cred); err != nil {
				return err
			} else if red {
				return httpapi.Errorf(http.StatusUnauthorized, nil, msgUnauthorized)
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		return f(w, r, u)
	})
}

// writeDoorkeeperError renders an OAuth resource error as Doorkeeper does
// for an API controller (head status plus headers).
func writeDoorkeeperError(w http.ResponseWriter, status int, name, desc string) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="Doorkeeper", error=%q, error_description=%q`, name, desc))
	w.WriteHeader(status)
}

// userRead guards the users/{id}/* stats-controller routes as Hackatime's
// set_user and ensure_public_stats_allowed! do. The caller is whoever a user
// API key (Bearer) or an OAuth token with the read scope names; anything
// else, including a red user's credentials, is anonymous rather than an
// error. "my" is the caller (404 without one). The target must exist (404)
// and have a public profile or be the caller (403).
func (h *Handler) userRead(f func(http.ResponseWriter, *http.Request, User) error) http.Handler {
	return httpapi.Handle(func(w http.ResponseWriter, r *http.Request) error {
		cred, err := userCredential(r.Context(), h.auth, r, false)
		if err != nil {
			return fmt.Errorf("checking credentials: %w", err)
		}
		if !h.allow(w, r, cred) {
			return nil
		}
		if !cred.anonymous() {
			if red, err := h.banned(r, cred); err != nil {
				return err
			} else if red {
				cred = credential{}
			}
		}
		var target User
		ident := r.PathValue("id")
		if classifyIdent(ident, true) == identMy {
			if cred.anonymous() {
				return httpapi.Errorf(http.StatusNotFound, nil, msgUserNotFound)
			}
			target, err = h.svc.UserByID(r.Context(), cred.userID)
		} else {
			target, err = h.svc.Lookup(r.Context(), ident)
		}
		if errors.Is(err, ErrUserNotFound) {
			return httpapi.Errorf(http.StatusNotFound, nil, msgUserNotFound)
		}
		if err != nil {
			return err
		}
		if !target.ProfilePublic && target.ID != cred.userID {
			return httpapi.Errorf(http.StatusForbidden, nil, msgPrivateStats)
		}
		return f(w, r, target)
	})
}

// public guards trust_factor, which Hackatime serves to anyone, by client.
func (h *Handler) public(f http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.allow(w, r, credential{}) {
			f(w, r)
		}
	})
}

// keyOrToken guards my/heartbeats as Hackatime's ensure_authenticated!:
// a user API key (Bearer or Basic) or an OAuth token with the read scope,
// else 401 {"error": "Unauthorized"}. A red user's credentials don't count.
func (h *Handler) keyOrToken(f func(http.ResponseWriter, *http.Request, User) error) http.Handler {
	return httpapi.Handle(func(w http.ResponseWriter, r *http.Request) error {
		cred, err := userCredential(r.Context(), h.auth, r, true)
		if err != nil {
			return fmt.Errorf("checking credentials: %w", err)
		}
		if !h.allow(w, r, cred) {
			return nil
		}
		if cred.anonymous() {
			return httpapi.Errorf(http.StatusUnauthorized, nil, msgUnauthorized)
		}
		if red, err := h.banned(r, cred); err != nil {
			return err
		} else if red {
			return httpapi.Errorf(http.StatusUnauthorized, nil, msgUnauthorized)
		}
		u, err := h.svc.UserByID(r.Context(), cred.userID)
		if errors.Is(err, ErrUserNotFound) {
			return httpapi.Errorf(http.StatusUnauthorized, nil, msgUnauthorized)
		}
		if err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "no-store")
		return f(w, r, u)
	})
}

// paramError turns a *ParamError into Hackatime's 422 {"error": "..."}.
func paramError(err error) error {
	var pe *ParamError
	if errors.As(err, &pe) {
		return httpapi.Errorf(http.StatusUnprocessableEntity, err, "%s", pe.Message)
	}
	return err
}

// setRangeCache marks a response about a range ending before now as
// cacheable for five minutes, and anything else as not cacheable.
func (h *Handler) setRangeCache(w http.ResponseWriter, end time.Time) {
	if end.Before(h.svc.Now()) {
		w.Header().Set("Cache-Control", closedRangeCache)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request, u User) error {
	body, err := h.svc.Me(r.Context(), u)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, body)
	return nil
}

func (h *Handler) authProjects(w http.ResponseWriter, r *http.Request, u User) error {
	q := AuthProjectsQuery(r.URL.Query(), h.svc.Now())
	if !validList(q.Names) {
		return httpapi.Errorf(http.StatusUnprocessableEntity, nil, "Invalid projects")
	}
	projects, err := h.svc.AuthProjects(r.Context(), u, q)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"projects": projects})
	return nil
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request, u User) error {
	req, err := ParseStatsRequest(r.URL.Query(), h.svc.Now())
	if err != nil {
		return paramError(err)
	}
	h.setRangeCache(w, req.Range.To)
	if req.TotalOnly {
		t, err := h.svc.StatsTotals(r.Context(), u, req)
		if err != nil {
			return err
		}
		httpapi.WriteJSON(w, http.StatusOK, t)
		return nil
	}
	body, err := h.svc.Stats(r.Context(), u, req)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, body)
	return nil
}

func (h *Handler) spans(w http.ResponseWriter, r *http.Request, u User) error {
	req, err := ParseSpansRequest(r.URL.Query(), h.svc.Now())
	if err != nil {
		return paramError(err)
	}
	st, err := h.svc.Settings(r.Context(), u.ID)
	if err != nil {
		return err
	}
	spans, err := h.svc.Spans(r.Context(), u.ID, req.From, req.To, req.Projects, st.Hidden)
	if err != nil {
		return err
	}
	h.setRangeCache(w, req.To)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"spans": spans})
	return nil
}

func (h *Handler) userProjects(w http.ResponseWriter, r *http.Request, u User) error {
	rng := projectDiscoveryRange(r.URL.Query(), h.svc.Now())
	names, err := h.svc.ProjectNames(r.Context(), u.ID, rng)
	if err != nil {
		return err
	}
	h.setRangeCache(w, rng.To)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"projects": names})
	return nil
}

func (h *Handler) userProject(w http.ResponseWriter, r *http.Request, u User) error {
	name := r.PathValue("name")
	if name == "" || !validList([]string{name}) {
		return httpapi.Errorf(http.StatusBadRequest, nil, "whats the name?")
	}
	q := UserProjectsQuery(r.URL.Query(), h.svc.Now(), []string{name})
	details, err := h.svc.ProjectDetails(r.Context(), u.ID, q)
	if err != nil {
		return err
	}
	if len(details) == 0 {
		return httpapi.Errorf(http.StatusNotFound, nil, msgProjectMissing)
	}
	h.setRangeCache(w, q.Range.To)
	httpapi.WriteJSON(w, http.StatusOK, details[0])
	return nil
}

func (h *Handler) userProjectsDetails(w http.ResponseWriter, r *http.Request, u User) error {
	qs := r.URL.Query()
	q := UserProjectsQuery(qs, h.svc.Now(), trimmedList(qs, "projects"))
	if !validList(q.Names) {
		return httpapi.Errorf(http.StatusUnprocessableEntity, nil, "Invalid projects")
	}
	details, err := h.svc.ProjectDetails(r.Context(), u.ID, q)
	if err != nil {
		return err
	}
	h.setRangeCache(w, q.Range.To)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"projects": details})
	return nil
}

func (h *Handler) trustFactor(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.Lookup(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrUserNotFound) {
		httpapi.WriteError(w, r, httpapi.Errorf(http.StatusNotFound, nil, msgUserNotFound))
		return
	}
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	body, err := h.svc.TrustFactor(r.Context(), u)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, body)
}

func (h *Handler) myHeartbeats(w http.ResponseWriter, r *http.Request, u User) error {
	req, err := ParseHeartbeatsRequest(r.URL.Query(), h.svc.Now())
	if err != nil {
		return paramError(err)
	}
	body, err := h.svc.MyHeartbeats(r.Context(), u, req)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, body)
	return nil
}
