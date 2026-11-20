package auth

import (
	"crypto/subtle"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// devHCAPrefix marks users created by dev sign-in. Real HCA ids never use it.
const devHCAPrefix = "dev:"

// devUsernameRe is what dev sign-in accepts (lowercased first).
var devUsernameRe = regexp.MustCompile(`^[a-z0-9_-]{2,32}$`)

// Callback failure codes, sent to the UI as /?auth_error=<code>.
const (
	authErrInvalidState = "invalid_state"
	authErrDenied       = "access_denied"
	authErrFailed       = "failed"
)

// Handler serves the auth routes.
type Handler struct{ s *Service }

// NewHandler returns the HTTP handler for s.
func NewHandler(s *Service) *Handler { return &Handler{s: s} }

// Register adds the auth routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	s := h.s
	perClient := httpapi.IPKey(s.trustProxy)
	signInLimited := s.signInLimiter.Middleware(perClient)
	limited := s.authLimiter.Middleware(perClient)

	mux.Handle("GET /auth/login", signInLimited(httpapi.Handle(h.login)))
	mux.Handle("GET /auth/callback", limited(httpapi.Handle(h.callback)))
	mux.Handle("POST /auth/logout", limited(s.RequireSession(httpapi.Handle(h.logout))))
	mux.Handle("POST /auth/dev-login", signInLimited(httpapi.Handle(h.devLogin)))

	mux.Handle("GET /api/v1/session", s.RequireSession(httpapi.Handle(h.session)))
	mux.Handle("GET /api/v1/api_keys", s.RequireSession(httpapi.Handle(h.listKeys)))
	mux.Handle("POST /api/v1/api_keys", s.RequireSession(httpapi.Handle(h.createKey)))
	mux.Handle("DELETE /api/v1/api_keys/{id}", s.RequireSession(httpapi.Handle(h.revokeKey)))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	s := h.s
	if !s.hca.configured() {
		return httpapi.Errorf(http.StatusServiceUnavailable, errHCANotConfigured, "sign-in is not configured")
	}
	st := oauthState{
		State:    randomToken(32),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Expires:  s.now().Add(stateTTL).Unix(),
	}
	v, err := s.encodeState(st)
	if err != nil {
		return err
	}
	s.setStateCookie(w, v)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.hca.authURL(st.State, st.Verifier), http.StatusFound)
	return nil
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) error {
	s := h.s
	w.Header().Set("Cache-Control", "no-store")
	c, err := r.Cookie(stateCookieName)
	if err != nil {
		h.failSignIn(w, r, authErrInvalidState, "no state cookie")
		return nil
	}
	// Clear the cookie whatever happens next, so this browser can't reuse it.
	// The state itself is stateless (a MAC'd cookie with a 10-minute
	// expiry); a captured cookie and state pair is stopped by HCA's
	// single-use code and our PKCE verifier, not by a server-side record.
	s.clearStateCookie(w)
	st, err := s.decodeState(c.Value)
	if err != nil {
		h.failSignIn(w, r, authErrInvalidState, "bad state cookie")
		return nil
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		h.failSignIn(w, r, authErrInvalidState, "state mismatch")
		return nil
	}
	if q.Get("error") != "" {
		h.failSignIn(w, r, authErrDenied, "provider returned an error")
		return nil
	}
	code := q.Get("code")
	if code == "" || len(code) > 1024 {
		h.failSignIn(w, r, authErrFailed, "missing code")
		return nil
	}
	id, err := s.hca.identity(r.Context(), code, st.Verifier)
	if err != nil {
		slog.WarnContext(r.Context(), "HCA sign-in failed", "err", err)
		h.failSignIn(w, r, authErrFailed, "identity exchange failed")
		return nil
	}
	if _, err := h.startSession(w, r, signInFromHCA(id), methodHCA); err != nil {
		return err
	}
	http.Redirect(w, r, s.webLocation(safeNext(st.Next)), http.StatusFound)
	return nil
}

// failSignIn sends the browser back to the UI with an error code.
func (h *Handler) failSignIn(w http.ResponseWriter, r *http.Request, code, reason string) {
	slog.InfoContext(r.Context(), "sign-in rejected", "code", code, "reason", reason)
	http.Redirect(w, r, h.s.webLocation("/?auth_error="+url.QueryEscape(code)), http.StatusFound)
}

// startSession signs the browser in through Service.signIn, passing the hash
// of any session cookie it already has so that session is replaced, then
// sets the new cookie.
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, in signInIdentity, method string) (signedIn, error) {
	old, _ := sessionHashFromRequest(r) // nil when absent or malformed
	out, err := h.s.signIn(r.Context(), in, method, old)
	if err != nil {
		return signedIn{}, err
	}
	h.s.setSessionCookie(w, out.cookie, out.sess.expiresAt)
	return out, nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	u, _ := httpapi.UserFrom(r.Context())
	sess, _ := sessionFrom(r.Context()) // RequireSession guarantees both
	if err := h.s.logout(r.Context(), u.ID, sess.idHash); err != nil {
		return err
	}
	h.s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// isLoopbackPeer reports whether a "host:port" remote address is a loopback IP.
func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type devLoginRequest struct {
	Username string `json:"username"`
}

func (h *Handler) devLogin(w http.ResponseWriter, r *http.Request) error {
	s := h.s
	// Config already requires a loopback base URL and listen address; also
	// refuse any connection that doesn't come from this machine. RemoteAddr
	// is the TCP peer, never a client-supplied header.
	if !s.devLogin || !isLoopbackPeer(r.RemoteAddr) {
		return httpapi.Errorf(http.StatusNotFound, nil, "not found")
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case err == nil && mt == "application/json":
		return h.devLoginJSON(w, r)
	case err == nil && mt == "application/x-www-form-urlencoded":
		return h.devLoginForm(w, r)
	}
	return httpapi.Errorf(http.StatusUnsupportedMediaType, nil,
		"Content-Type must be application/json or application/x-www-form-urlencoded")
}

// devLoginJSON signs in from a script and answers like GET /api/v1/session.
// A cross-site page can't send a JSON body without a CORS preflight, and
// /auth/* answers none, which blocks login CSRF.
func (h *Handler) devLoginJSON(w http.ResponseWriter, r *http.Request) error {
	var req devLoginRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	out, err := h.devSignIn(w, r, req.Username)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{
		User: toUserJSON(out.user), CSRFToken: out.sess.csrfToken, ExpiresAt: out.sess.expiresAt,
	})
	return nil
}

// maxDevFormBody bounds the dev sign-in form (a username and a next path).
const maxDevFormBody = 4 << 10

// devLoginForm signs in from a form on the web UI, which lives on another
// origin (ADR 0011), and sends the browser back to it with 303. Forms can be
// posted cross-site, so the request's Origin must be the web origin; a form
// on any other page gets 403 (login CSRF).
func (h *Handler) devLoginForm(w http.ResponseWriter, r *http.Request) error {
	s := h.s
	if s.webOrigin == "" || r.Header.Get("Origin") != s.webOrigin {
		return httpapi.Errorf(http.StatusForbidden, nil, "form sign-in is only accepted from the web UI")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDevFormBody)
	if err := r.ParseForm(); err != nil {
		return httpapi.Errorf(http.StatusBadRequest, err, "invalid form body")
	}
	if _, err := h.devSignIn(w, r, r.PostForm.Get("username")); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	// webLocation joins the configured web origin with a safeNext path, which
	// always starts with exactly one "/", so the host can't be changed.
	http.Redirect(w, r, s.webLocation(safeNext(r.PostForm.Get("next"))), http.StatusSeeOther) //nolint:gosec // see above
	return nil
}

// devSignIn validates username and signs the browser in as that dev user.
func (h *Handler) devSignIn(w http.ResponseWriter, r *http.Request, username string) (signedIn, error) {
	name := strings.ToLower(strings.TrimSpace(username))
	if !devUsernameRe.MatchString(name) {
		return signedIn{}, httpapi.Errorf(http.StatusBadRequest, nil, "username must match [a-z0-9_-]{2,32}")
	}
	return h.startSession(w, r, signInIdentity{hcaID: devHCAPrefix + name, username: name, displayName: name}, methodDev)
}

// userJSON is the signed-in user's own view of their account.
type userJSON struct {
	ID            uuid.UUID `json:"id"`
	Username      string    `json:"username"`
	DisplayName   string    `json:"display_name"`
	Email         string    `json:"email"`
	AvatarURL     string    `json:"avatar_url"`
	Timezone      string    `json:"timezone"`
	ProfilePublic bool      `json:"profile_public"`
	Role          string    `json:"role"`
	CreatedAt     time.Time `json:"created_at"`
}

func toUserJSON(u db.User) userJSON {
	return userJSON{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Email: u.Email,
		AvatarURL: u.AvatarUrl, Timezone: u.Timezone, ProfilePublic: u.ProfilePublic,
		Role: u.Role, CreatedAt: u.CreatedAt,
	}
}

type sessionResponse struct {
	User      userJSON  `json:"user"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *Handler) session(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	cu, _ := httpapi.UserFrom(ctx)
	sess, _ := sessionFrom(ctx) // RequireSession guarantees both
	u, err := h.s.currentUser(ctx, cu.ID)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{User: toUserJSON(u), CSRFToken: sess.csrfToken, ExpiresAt: sess.expiresAt})
	return nil
}

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) error {
	u, _ := httpapi.UserFrom(r.Context())
	keys, err := h.s.ListAPIKeys(r.Context(), u.ID)
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"api_keys": keys})
	return nil
}

type createKeyRequest struct {
	Name string `json:"name"`
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) error {
	u, _ := httpapi.UserFrom(r.Context())
	var req createKeyRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	k, err := h.s.CreateAPIKey(r.Context(), u.ID, req.Name)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusCreated, k)
	return nil
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) error {
	u, _ := httpapi.UserFrom(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return errKeyNotFound
	}
	if err := h.s.RevokeAPIKey(r.Context(), u.ID, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
