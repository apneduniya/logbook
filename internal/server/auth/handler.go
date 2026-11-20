package auth

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"mime"
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
	limited := s.authLimiter.Middleware(httpapi.IPKey(s.trustProxy))

	mux.Handle("GET /auth/login", limited(httpapi.Handle(h.login)))
	mux.Handle("GET /auth/callback", limited(httpapi.Handle(h.callback)))
	mux.Handle("POST /auth/logout", limited(s.RequireSession(httpapi.Handle(h.logout))))
	mux.Handle("POST /auth/dev-login", limited(httpapi.Handle(h.devLogin)))

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
	s.clearStateCookie(w) // single use, whatever happens next
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
	if _, _, err := h.signIn(w, r, signInFromHCA(id), "hca"); err != nil {
		return err
	}
	http.Redirect(w, r, st.Next, http.StatusFound)
	return nil
}

// failSignIn sends the browser back to the UI with an error code.
func (h *Handler) failSignIn(w http.ResponseWriter, r *http.Request, code, reason string) {
	slog.InfoContext(r.Context(), "sign-in rejected", "code", code, "reason", reason)
	http.Redirect(w, r, "/?auth_error="+url.QueryEscape(code), http.StatusFound)
}

// signIn upserts the user, replaces any existing session on this browser
// with a new one (no session fixation), writes the audit row and sets the
// session cookie.
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request, in signIn, method string) (db.User, session, error) {
	s := h.s
	ctx := r.Context()
	u, err := s.upsertUser(ctx, in)
	if err != nil {
		return db.User{}, session{}, err
	}
	var cookie string
	var sess session
	err = s.st.InTx(ctx, func(q *db.Queries) error {
		if old, ok := sessionHashFromRequest(r); ok {
			if err := q.DeleteSession(ctx, old); err != nil {
				return fmt.Errorf("deleting previous session: %w", err)
			}
		}
		var cerr error
		cookie, sess, cerr = s.createSession(ctx, q, u.ID)
		if cerr != nil {
			return cerr
		}
		return audit(ctx, q, u.ID, "auth.login", "user", u.ID.String(), map[string]any{"method": method})
	})
	if err != nil {
		return db.User{}, session{}, err
	}
	s.setSessionCookie(w, cookie, sess.expiresAt)
	return u, sess, nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	s := h.s
	ctx := r.Context()
	u, _ := httpapi.UserFrom(ctx)
	sess, _ := sessionFrom(ctx) // RequireSession guarantees both
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		if err := q.DeleteSession(ctx, sess.idHash); err != nil {
			return fmt.Errorf("deleting session: %w", err)
		}
		return audit(ctx, q, u.ID, "auth.logout", "user", u.ID.String(), nil)
	})
	if err != nil {
		return err
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type devLoginRequest struct {
	Username string `json:"username"`
}

func (h *Handler) devLogin(w http.ResponseWriter, r *http.Request) error {
	s := h.s
	if !s.devLogin {
		return httpapi.Errorf(http.StatusNotFound, nil, "not found")
	}
	// Requiring JSON blocks cross-site form posts (login CSRF).
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return httpapi.Errorf(http.StatusUnsupportedMediaType, nil, "Content-Type must be application/json")
	}
	var req devLoginRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	name := strings.ToLower(strings.TrimSpace(req.Username))
	if !devUsernameRe.MatchString(name) {
		return httpapi.Errorf(http.StatusBadRequest, nil, "username must match [a-z0-9_-]{2,32}")
	}
	u, sess, err := h.signIn(w, r, signIn{hcaID: devHCAPrefix + name, username: name, displayName: name}, "dev")
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{User: toUserJSON(u), CSRFToken: sess.csrfToken, ExpiresAt: sess.expiresAt})
	return nil
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
	u, err := h.s.st.GetUserByID(ctx, cu.ID)
	if err != nil {
		return fmt.Errorf("loading session user: %w", err)
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
