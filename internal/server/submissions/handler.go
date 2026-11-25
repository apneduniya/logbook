package submissions

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/pkg/submission"
)

// Rate limits (docs/api/submissions.md). Limiters keep at most
// limiterMaxKeys buckets.
const (
	// Uploads (a submission or a part) per user: a burst of 10, refilled at
	// 10 an hour.
	uploadBurst = 10
	// Plans per user: a burst of 60, refilled at one a second.
	planBurst = 60
	// Review link reads per client: a burst of 120, refilled at 120 a minute.
	reviewBurst = 120
	// Decisions per reviewer: a burst of 30, refilled at 30 a minute.
	decisionBurst = 30
	// Program API reads per token: a burst of 600, refilled at 600 a
	// minute (ADR 0021).
	programBurst   = 600
	limiterMaxKeys = 10_000
	limiterIdleTTL = time.Hour
)

// HandlerOptions are the middleware the routes need.
type HandlerOptions struct {
	// RequireSession authenticates browser calls (CSRF on writes).
	RequireSession httpapi.Middleware
	// RequireAPIKey authenticates the agent.
	RequireAPIKey httpapi.Middleware
	// RequireSubmissionsScope accepts an OAuth access token with the
	// submissions scope (oauth.Service.RequireScope); only an app's
	// client-credentials token has it.
	RequireSubmissionsScope httpapi.Middleware
	// ClientKey keys the review link's per-client limit.
	ClientKey func(*http.Request) string
	// UploadSlots is how many upload bodies are decoded at once (default
	// 2), and UploadWait how long a body waits for a slot (default a
	// minute) before it is refused with 503.
	UploadSlots int
	UploadWait  time.Duration
}

// Handler serves the submission routes.
type Handler struct {
	svc     *Service
	opts    HandlerOptions
	uploads *uploadGate

	uploadLimit, planLimit, reviewLimit, decisionLimit, programLimit httpapi.Middleware
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, opts HandlerOptions) *Handler {
	limiter := func(burst int, per time.Duration) *httpapi.RateLimiter {
		return httpapi.NewRateLimiter(float64(burst)/per.Seconds(), burst, limiterMaxKeys, limiterIdleTTL)
	}
	return &Handler{
		svc: svc, opts: opts, uploads: newUploadGate(opts.UploadSlots, opts.UploadWait),
		uploadLimit:   limiter(uploadBurst, time.Hour).Middleware(httpapi.UserKey),
		planLimit:     limiter(planBurst, time.Minute).Middleware(httpapi.UserKey),
		reviewLimit:   limiter(reviewBurst, time.Minute).Middleware(opts.ClientKey),
		decisionLimit: limiter(decisionBurst, time.Minute).Middleware(httpapi.UserKey),
		programLimit:  limiter(programBurst, time.Minute).Middleware(httpapi.OAuthTokenKey),
	}
}

// Register mounts the routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	o := h.opts
	// The agent (API key).
	mux.Handle("GET /api/v1/me/submissions/plan", httpapi.Chain(httpapi.Handle(h.plan), o.RequireAPIKey, h.planLimit))
	mux.Handle("POST /api/v1/submissions", httpapi.Chain(httpapi.Handle(h.create), o.RequireAPIKey, h.uploadLimit))
	mux.Handle("POST /api/v1/submissions/{id}/parts", httpapi.Chain(httpapi.Handle(h.addPart), o.RequireAPIKey, h.uploadLimit))

	// The submitter (session).
	mux.Handle("GET /api/v1/me/submissions", httpapi.Chain(httpapi.Handle(h.mine), noStore, o.RequireSession))
	mux.Handle("GET /api/v1/me/submissions/{id}", httpapi.Chain(httpapi.Handle(h.mineOne), noStore, o.RequireSession))
	mux.Handle("DELETE /api/v1/me/submissions/{id}", httpapi.Chain(httpapi.Handle(h.withdraw), noStore, o.RequireSession))

	// The review link (the token is the credential) and decisions.
	mux.Handle("GET /api/v1/review/{token}", httpapi.Chain(httpapi.Handle(h.review), reviewHeaders, h.reviewLimit))
	mux.Handle("POST /api/v1/review/{token}/decision",
		httpapi.Chain(httpapi.Handle(h.decide), reviewHeaders, o.RequireSession, h.decisionLimit))

	// The program's inbox: its members (session) and its own client token.
	mux.Handle("GET /api/v1/me/apps/{id}/submissions", httpapi.Chain(httpapi.Handle(h.ofApp), noStore, o.RequireSession))
	mux.Handle("GET /api/v1/submissions", httpapi.Chain(httpapi.Handle(h.ofProgram), noStore, o.RequireSubmissionsScope, h.programLimit))
	mux.Handle("GET /api/v1/submissions/{id}", httpapi.Chain(httpapi.Handle(h.ofProgramOne), noStore, o.RequireSubmissionsScope, h.programLimit))
}

// noStore keeps responses that carry review links out of caches.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// reviewHeaders keeps the review link out of caches, referrers and search
// engines (ADR 0020), on every answer including errors.
func reviewHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) (uuid.UUID, error) {
	u, ok := httpapi.UserFrom(r.Context())
	if !ok {
		return uuid.Nil, httpapi.Errorf(http.StatusUnauthorized, nil, "unauthorized")
	}
	return u.ID, nil
}

// pathID parses the {id} path value; anything that isn't a UUID is
// notFound.
func pathID(r *http.Request, notFound error) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, mapError(notFound)
	}
	return id, nil
}

func (h *Handler) plan(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	p, err := h.svc.Plan(r.Context(), user, PlanQuery{
		Project: q.Get("project"), From: q.Get("from"), To: q.Get("to"), Device: q.Get("device"),
		Submission: q.Get("submission"),
	})
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	var out Accepted
	err = h.upload(w, r, user, func(body []byte) error {
		var up submission.Upload
		if err := decodeGzipJSON(body, h.svc.maxJSON, &up); err != nil {
			return err
		}
		out, err = h.svc.Create(r.Context(), user, up)
		return mapError(err)
	})
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusAccepted, out)
	return nil
}

func (h *Handler) addPart(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, ErrNotFound)
	if err != nil {
		return err
	}
	var out Accepted
	err = h.upload(w, r, user, func(body []byte) error {
		var part submission.Part
		if err := decodeGzipJSON(body, h.svc.maxJSON, &part); err != nil {
			return err
		}
		out, err = h.svc.AddPart(r.Context(), user, id, part)
		return mapError(err)
	})
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusAccepted, out)
	return nil
}

// upload runs fn on an upload's compressed body within the gate: one
// upload per user at a time, the body read in full, then fn (decoding and
// storing it) once a decode slot is free.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, user uuid.UUID, fn func(body []byte) error) error {
	leave, err := h.uploads.enter(w, user)
	if err != nil {
		return err
	}
	defer leave()
	body, err := readGzipBody(w, r, h.svc.maxGzip)
	if err != nil {
		return err
	}
	release, err := h.uploads.slot(r.Context(), w)
	if err != nil {
		return err
	}
	defer release()
	return fn(body)
}

func (h *Handler) mine(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	out, err := h.svc.Mine(r.Context(), user)
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"submissions": out})
	return nil
}

func (h *Handler) mineOne(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, ErrNotFound)
	if err != nil {
		return err
	}
	out, err := h.svc.MineOne(r.Context(), user, id)
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) withdraw(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, ErrNotFound)
	if err != nil {
		return err
	}
	if err := h.svc.Withdraw(r.Context(), user, id); err != nil {
		return mapError(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) review(w http.ResponseWriter, r *http.Request) error {
	out, err := h.svc.Review(r.Context(), r.PathValue("token"))
	if err != nil {
		return mapError(err)
	}
	if out.Submission.Status == StatusVerifying {
		httpapi.WriteJSON(w, http.StatusAccepted, map[string]any{"submission": out.Submission})
		return nil
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	var in DecisionInput
	if err := httpapi.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	out, err := h.svc.Decide(r.Context(), user, r.PathValue("token"), in)
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"decision": out})
	return nil
}

// programFilter parses status, decision, limit and offset.
func programFilter(r *http.Request) (ProgramFilter, error) {
	q := r.URL.Query()
	f := ProgramFilter{Status: q.Get("status"), Decision: q.Get("decision")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, httpapi.Errorf(http.StatusBadRequest, err, "limit must be a number")
		}
		f.Limit = &n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, httpapi.Errorf(http.StatusBadRequest, err, "offset must be a number")
		}
		f.Offset = n
	}
	return f, nil
}

func (h *Handler) ofApp(w http.ResponseWriter, r *http.Request) error {
	user, err := currentUser(r)
	if err != nil {
		return err
	}
	app, err := pathID(r, errAppNotFound)
	if err != nil {
		return err
	}
	f, err := programFilter(r)
	if err != nil {
		return err
	}
	out, err := h.svc.OfApp(r.Context(), user, app, f)
	if errors.Is(err, ErrNotFound) {
		err = errAppNotFound
	}
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"submissions": out})
	return nil
}

// errAppNotFound is the answer for an app the caller isn't a member of, as
// the oauth package gives it.
var errAppNotFound = httpapi.Errorf(http.StatusNotFound, nil, "not found")

// appOfToken is the app whose client-credentials token made the request.
func appOfToken(r *http.Request) (uuid.UUID, error) {
	t, ok := httpapi.OAuthTokenFrom(r.Context())
	if !ok || !t.IsClient() {
		return uuid.Nil, httpapi.Errorf(http.StatusUnauthorized, nil, "unauthorized")
	}
	return t.AppID, nil
}

func (h *Handler) ofProgram(w http.ResponseWriter, r *http.Request) error {
	app, err := appOfToken(r)
	if err != nil {
		return err
	}
	f, err := programFilter(r)
	if err != nil {
		return err
	}
	out, err := h.svc.OfProgram(r.Context(), app, f)
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"submissions": out})
	return nil
}

func (h *Handler) ofProgramOne(w http.ResponseWriter, r *http.Request) error {
	app, err := appOfToken(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, ErrNotFound)
	if err != nil {
		return err
	}
	out, err := h.svc.OfProgramOne(r.Context(), app, id)
	if err != nil {
		return mapError(err)
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
	return nil
}

// mapError turns service errors into HTTP errors.
func mapError(err error) error {
	var he *httpapi.Error
	switch {
	case errors.As(err, &he):
		return he
	case errors.Is(err, ErrNotFound):
		return httpapi.Errorf(http.StatusNotFound, err, "submission not found")
	case errors.Is(err, ErrProgramNotFound):
		return httpapi.Errorf(http.StatusNotFound, err, "program not found")
	case errors.Is(err, ErrBanned):
		return httpapi.Errorf(http.StatusForbidden, err, "%s", ErrBanned.Error())
	case errors.Is(err, ErrNothingToSubmit):
		return httpapi.Errorf(http.StatusConflict, err, "%s", ErrNothingToSubmit.Error())
	case errors.Is(err, ErrDecided):
		return httpapi.Errorf(http.StatusConflict, err, "%s", ErrDecided.Error())
	case errors.Is(err, ErrWithdrawn):
		return httpapi.Errorf(http.StatusGone, err, "%s", ErrWithdrawn.Error())
	case errors.Is(err, ErrInvalid):
		return httpapi.Errorf(http.StatusBadRequest, err, "%s", detail(err, ErrInvalid))
	case errors.Is(err, ErrConflict):
		return httpapi.Errorf(http.StatusConflict, err, "%s", detail(err, ErrConflict))
	case errors.Is(err, ErrForbidden):
		return httpapi.Errorf(http.StatusForbidden, err, "%s", detail(err, ErrForbidden))
	}
	return err
}

// detail is a wrapped error's message without its sentinel's.
func detail(err, sentinel error) string {
	return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
}
