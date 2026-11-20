package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

const (
	// SessionCookie holds the raw session id; only its SHA-256 is stored.
	SessionCookie = "logbook_session"
	// CSRFHeader carries the session's CSRF token on unsafe requests.
	CSRFHeader = "X-CSRF-Token"

	sessionIDBytes = 32
	csrfBytes      = 32
)

// session is the validated session attached to a request context.
type session struct {
	idHash    []byte
	csrfToken string
	expiresAt time.Time
}

type sessionKey struct{}

func withSession(ctx context.Context, s session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

func sessionFrom(ctx context.Context) (session, bool) {
	s, ok := ctx.Value(sessionKey{}).(session)
	return s, ok
}

func hashToken(raw []byte) []byte {
	h := sha256.Sum256(raw)
	return h[:]
}

// createSession stores a new session for userID and returns the cookie value.
func (s *Service) createSession(ctx context.Context, q db.Querier, userID uuid.UUID) (cookie string, sess session, err error) {
	raw := make([]byte, sessionIDBytes)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error
	now := s.now()
	sess = session{
		idHash:    hashToken(raw),
		csrfToken: randomToken(csrfBytes),
		expiresAt: now.Add(s.sessionTTL),
	}
	if err := q.CreateSession(ctx, db.CreateSessionParams{
		IDHash: sess.idHash, UserID: userID, CsrfToken: sess.csrfToken,
		CreatedAt: now, ExpiresAt: sess.expiresAt,
	}); err != nil {
		return "", session{}, fmt.Errorf("creating session: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), sess, nil
}

func (s *Service) setSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	//nolint:gosec // G124: Secure is on unless LOGBOOK_DEV_INSECURE_COOKIE is set for plain-http dev
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   max(1, int(expires.Sub(s.now())/time.Second)),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearSessionCookie(w http.ResponseWriter) {
	//nolint:gosec // G124: Secure is on unless LOGBOOK_DEV_INSECURE_COOKIE is set for plain-http dev
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// sessionHashFromRequest decodes the session cookie. ok is false when the
// cookie is missing or malformed.
func sessionHashFromRequest(r *http.Request) (hash []byte, ok bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(raw) != sessionIDBytes {
		return nil, false
	}
	return hashToken(raw), true
}

// loadSession resolves the request's session cookie. It slides the expiry
// forward at most once per sessionTouchEvery and re-issues the cookie when it
// does. found is false when there is no valid, unexpired session.
func (s *Service) loadSession(w http.ResponseWriter, r *http.Request) (u httpapi.CurrentUser, sess session, found bool, err error) {
	hash, ok := sessionHashFromRequest(r)
	if !ok {
		return u, sess, false, nil
	}
	ctx := r.Context()
	now := s.now()
	row, err := s.st.GetActiveSession(ctx, db.GetActiveSessionParams{IDHash: hash, ExpiresAt: now})
	if store.IsNotFound(err) {
		return u, sess, false, nil
	}
	if err != nil {
		return u, sess, false, fmt.Errorf("loading session: %w", err)
	}
	sess = session{idHash: row.IDHash, csrfToken: row.CsrfToken, expiresAt: row.ExpiresAt}
	if now.Sub(row.LastSeenAt) >= sessionTouchEvery {
		newExpiry := now.Add(s.sessionTTL)
		n, err := s.st.TouchSession(ctx, db.TouchSessionParams{
			SeenAt: now, ExpiresAt: newExpiry, IDHash: hash, TouchBefore: now.Add(-sessionTouchEvery),
		})
		if err != nil {
			return u, sess, false, fmt.Errorf("touching session: %w", err)
		}
		if n > 0 {
			sess.expiresAt = newExpiry
			c, _ := r.Cookie(SessionCookie) // present: decoded above
			s.setSessionCookie(w, c.Value, newExpiry)
		}
	}
	u = httpapi.CurrentUser{ID: row.UserID, Username: row.Username, Role: row.Role, Via: "session"}
	return u, sess, true, nil
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func csrfOK(r *http.Request, sess session) bool {
	got := r.Header.Get(CSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrfToken)) == 1
}

var (
	errNotSignedIn = httpapi.Errorf(http.StatusUnauthorized, nil, "not signed in")
	errBadCSRF     = httpapi.Errorf(http.StatusForbidden, nil, "missing or invalid CSRF token")
)

// RequireSession lets a request through only with a valid session cookie
// (401 otherwise) and, on methods other than GET/HEAD/OPTIONS, a matching
// X-CSRF-Token header (403 otherwise).
func (s *Service) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, sess, found, err := s.loadSession(w, r)
		switch {
		case err != nil:
			httpapi.WriteError(w, r, err)
		case !found:
			httpapi.WriteError(w, r, errNotSignedIn)
		case !isSafeMethod(r.Method) && !csrfOK(r, sess):
			httpapi.WriteError(w, r, errBadCSRF)
		default:
			ctx := withSession(httpapi.WithUser(r.Context(), u), sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		}
	})
}

// OptionalSession attaches the session user when there is one and passes
// anonymous requests through. On unsafe methods a session without a valid
// CSRF token is ignored, so the request proceeds as anonymous.
func (s *Service) OptionalSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, sess, found, err := s.loadSession(w, r)
		if err != nil {
			httpapi.WriteError(w, r, err)
			return
		}
		if found && (isSafeMethod(r.Method) || csrfOK(r, sess)) {
			r = r.WithContext(withSession(httpapi.WithUser(r.Context(), u), sess))
		}
		next.ServeHTTP(w, r)
	})
}

// CleanupExpiredSessions deletes sessions past their expiry and returns how
// many were removed.
func (s *Service) CleanupExpiredSessions(ctx context.Context) (int64, error) {
	n, err := s.st.DeleteExpiredSessions(ctx, s.now())
	if err != nil {
		return 0, fmt.Errorf("deleting expired sessions: %w", err)
	}
	return n, nil
}
