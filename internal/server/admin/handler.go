package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// Rate limits (docs/api/admin.md): per key, a burst of 300 refilled at 300
// a minute; per client, failed authentications, a burst of 30 refilled at
// one every two seconds.
const (
	RateBurst      = 300
	RatePerMinute  = 300
	failBurst      = 30
	failPerSecond  = 0.5
	limiterMaxKeys = 10_000
)

// msgUnauthorized is the error for a request without a live admin key.
// It is the text of Hackatime's token authentication failure.
const msgUnauthorized = "HTTP Token: Access denied."

// Auditor resolves admin keys and writes the admin.read rows; *Keys
// implements it.
type Auditor interface {
	Authenticate(ctx context.Context, key string) (Admin, error)
	AuditRead(ctx context.Context, a Admin, subjects []uuid.UUID, data map[string]any) error
}

// HandlerOptions configures a Handler.
type HandlerOptions struct {
	// Limiter limits each key; nil means a new one at RateBurst.
	Limiter *httpapi.RateLimiter
	// FailLimiter limits failed authentications per client; nil means a
	// new one.
	FailLimiter *httpapi.RateLimiter
	// ClientKey turns a request into its client's rate-limit key; nil
	// means httpapi.IPKey(false).
	ClientKey func(*http.Request) string
}

// NewRateLimiter returns the per-key limiter.
func NewRateLimiter() *httpapi.RateLimiter {
	return httpapi.NewRateLimiter(RatePerMinute/60.0, RateBurst, limiterMaxKeys, 10*time.Minute)
}

// Handler serves /api/admin/v1/.
type Handler struct {
	svc       *Service
	auth      Auditor
	limiter   *httpapi.RateLimiter
	fails     *httpapi.RateLimiter
	clientKey func(*http.Request) string
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, auth Auditor, opts HandlerOptions) *Handler {
	if opts.Limiter == nil {
		opts.Limiter = NewRateLimiter()
	}
	if opts.FailLimiter == nil {
		opts.FailLimiter = httpapi.NewRateLimiter(failPerSecond, failBurst, limiterMaxKeys, 10*time.Minute)
	}
	if opts.ClientKey == nil {
		opts.ClientKey = httpapi.IPKey(false)
	}
	return &Handler{svc: svc, auth: auth, limiter: opts.Limiter, fails: opts.FailLimiter, clientKey: opts.ClientKey}
}

// Prefix is where the admin API lives.
const Prefix = "/api/admin/v1/"

// Register mounts the routes on mux. Every other path under /api/admin/
// is 404 (after authentication), whatever its method.
func (h *Handler) Register(mux *http.ServeMux) {
	// Hackatime-compatible (user_utilities.rb at 43bde10).
	mux.Handle("GET "+Prefix+"user/info", h.route(h.userInfo))
	mux.Handle("GET "+Prefix+"user/info_batch", h.route(h.userInfoBatch))
	mux.Handle("GET "+Prefix+"user/heartbeats", h.route(h.userHeartbeats))
	mux.Handle("GET "+Prefix+"user/projects", h.route(h.userProjects))
	mux.Handle("GET "+Prefix+"user/trust_logs", h.route(h.trustLogs))
	mux.Handle("POST "+Prefix+"user/get_user_by_email", h.route(h.userByEmail))
	mux.Handle("POST "+Prefix+"user/search_fuzzy", h.route(h.searchFuzzy))
	// Logbook's own.
	mux.Handle("GET "+Prefix+"user/devices", h.route(h.userDevices))
	mux.Handle("GET "+Prefix+"user/minutes", h.route(h.userMinutes))
	mux.Handle("GET "+Prefix+"user/submissions", h.route(h.userSubmissions))
	mux.Handle("GET "+Prefix+"user/audit_log", h.route(h.userAuditLog))
	mux.Handle("GET "+Prefix+"submissions/{id}", h.route(h.submission))
	mux.Handle("GET "+Prefix+"submissions/{id}/evidence", h.route(h.submissionEvidence))
	mux.Handle("GET "+Prefix+"submissions/{id}/bundle", h.route(h.submissionBundle))
	mux.Handle("/api/admin/", h.route(func(http.ResponseWriter, *http.Request, *call) error {
		return httpapi.Errorf(http.StatusNotFound, nil, "not found")
	}))
}

// call is one authenticated admin request.
type call struct {
	admin   Admin
	p       params
	auth    Auditor
	r       *http.Request
	audited bool
	// status is the error status of a request audited after it failed.
	status int
}

// audit writes the request's admin.read rows: one per user it reads (none
// means the request read no user). Handlers call it once they know whom
// they read and before they write anything; the route writes it for them
// when they fail first.
func (c *call) audit(ctx context.Context, subjects ...uuid.UUID) error {
	c.audited = true
	data := map[string]any{
		"method": c.r.Method, "path": c.r.URL.Path, "key": c.admin.KeyPrefix, "params": map[string]string(c.p),
	}
	if c.status != 0 {
		data["status"] = c.status
	}
	return c.auth.AuditRead(ctx, c.admin, subjects, data)
}

// route authenticates an admin key (Bearer), limits it, reads the
// parameters and runs f. A request f answers with an error before auditing
// is audited with the error's status. Requests the limit refuses read
// nothing and aren't audited.
func (h *Handler) route(f func(http.ResponseWriter, *http.Request, *call) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx := r.Context()
		key, ok := bearer(r)
		var a Admin
		var err error
		if ok {
			a, err = h.auth.Authenticate(ctx, key)
		}
		switch {
		case !ok || errors.Is(err, ErrInvalidKey):
			ck := "ip:" + h.clientKey(r)
			if !h.fails.Allow(ck) {
				httpapi.WriteTooManyRequests(w, r, h.fails.RetryAfter(ck))
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="Logbook admin"`)
			httpapi.WriteError(w, r, httpapi.Errorf(http.StatusUnauthorized, nil, msgUnauthorized))
			return
		case err != nil:
			httpapi.WriteError(w, r, err)
			return
		}
		lk := "key:" + a.KeyID.String()
		if !h.limiter.Allow(lk) {
			httpapi.WriteTooManyRequests(w, r, h.limiter.RetryAfter(lk))
			return
		}
		c := &call{admin: a, auth: h.auth, r: r}
		if c.p, err = readParams(r); err != nil {
			c.p = params{}
			err = paramsError(err)
		} else {
			err = f(w, r, c)
		}
		if err == nil {
			return
		}
		if !c.audited {
			c.status = http.StatusInternalServerError
			if he := (*httpapi.Error)(nil); errors.As(err, &he) {
				c.status = he.Status
			}
			if aerr := c.audit(ctx); aerr != nil {
				slog.ErrorContext(ctx, "auditing a failed admin request", "err", aerr, "path", r.URL.Path)
				err = aerr
			}
		}
		httpapi.WriteError(w, r, err)
	})
}

func paramsError(err error) error {
	if errors.Is(err, errBodyTooLarge) {
		return httpapi.Errorf(http.StatusRequestEntityTooLarge, err, "%s", err.Error())
	}
	if errors.Is(err, errBadJSON) || errors.Is(err, errBadForm) {
		return httpapi.Errorf(http.StatusBadRequest, err, "%s", err.Error())
	}
	return err
}

// bearer returns the request's Bearer credential. Cookies, the api_key
// query parameter and Basic credentials are never read.
func bearer(r *http.Request) (string, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, cred, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	cred = strings.TrimSpace(cred)
	return cred, cred != ""
}

// Hackatime's errors (render_helpers.rb: render_error is 422).
func unprocessable(msg string) error {
	return httpapi.Errorf(http.StatusUnprocessableEntity, nil, "%s", msg)
}

func notFound(msg string) error {
	return httpapi.Errorf(http.StatusNotFound, nil, "%s", msg)
}

// errUserNotFound is find_user_by_id's 404.
var errUserNotFound = notFound("user not found")

// user resolves find_user_by_id's id (params[:id] || params[:user_id]):
// 422 "who?" when blank, 404 "user not found" when no user has that
// public id.
func (h *Handler) user(ctx context.Context, c *call) (UserRow, error) {
	raw, ok := c.p["id"]
	if !ok {
		raw = c.p["user_id"]
	}
	if strings.TrimSpace(raw) == "" {
		return UserRow{}, unprocessable("who?")
	}
	id, ok := railsID(raw)
	if !ok {
		return UserRow{}, errUserNotFound
	}
	u, err := h.svc.User(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return UserRow{}, errUserNotFound
	}
	return u, err
}

// writeJSON audits the request with subjects, then writes v.
func writeJSON(w http.ResponseWriter, r *http.Request, c *call, v any, subjects ...uuid.UUID) error {
	if err := c.audit(r.Context(), subjects...); err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, v)
	return nil
}
