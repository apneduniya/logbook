package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

const (
	maxAPIKeyName = 64
	// apiKeyPrefixLen is how much of a key we keep in clear to help users
	// tell keys apart. 8 hex characters of a random UUID reveal 32 of its
	// 122 random bits, leaving 90 unknown.
	apiKeyPrefixLen = 8
)

// apiKeyRe is the key format stock wakatime-cli accepts (pkg/params:
// apiKeyRegex): a lowercase UUIDv4, optionally prefixed with "waka_". We
// issue plain UUIDv4 keys.
var apiKeyRe = regexp.MustCompile(`^(waka_)?[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

var (
	errKeyNameInvalid = httpapi.Errorf(http.StatusBadRequest, nil,
		"name must be 1-%d characters with no control characters", maxAPIKeyName)
	errTooManyKeys = httpapi.Errorf(http.StatusConflict, nil,
		"you already have %d active API keys; revoke one first", MaxActiveAPIKeys)
	errKeyNotFound   = httpapi.Errorf(http.StatusNotFound, nil, "API key not found")
	errInvalidAPIKey = httpapi.Errorf(http.StatusUnauthorized, nil, "invalid API key")
)

// APIKey is an API key as shown to its owner. The secret is never included.
type APIKey struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// NewAPIKey is returned once, when a key is created.
type NewAPIKey struct {
	APIKey
	Key string `json:"key"`
}

func toAPIKey(k db.ApiKey) APIKey {
	return APIKey{ID: k.ID, Name: k.Name, Prefix: k.KeyPrefix, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt}
}

func validKeyName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxAPIKeyName {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return "", false
		}
	}
	return name, true
}

// CreateAPIKey issues a new key for userID. The plaintext key is in the
// result and is not stored anywhere.
func (s *Service) CreateAPIKey(ctx context.Context, userID uuid.UUID, name string) (NewAPIKey, error) {
	name, ok := validKeyName(name)
	if !ok {
		return NewAPIKey{}, errKeyNameInvalid
	}
	id, err := uuid.NewRandom() // crypto/rand-backed UUIDv4, lowercase when formatted
	if err != nil {
		return NewAPIKey{}, fmt.Errorf("generating API key: %w", err)
	}
	key := id.String()
	var created db.ApiKey
	err = s.st.InTx(ctx, func(q *db.Queries) error {
		// Serialise key creation per user so the cap can't be raced.
		if err := q.LockUser(ctx, userID); err != nil {
			return fmt.Errorf("locking user: %w", err)
		}
		n, err := q.CountActiveAPIKeys(ctx, userID)
		if err != nil {
			return fmt.Errorf("counting API keys: %w", err)
		}
		if n >= MaxActiveAPIKeys {
			return errTooManyKeys
		}
		created, err = q.CreateAPIKey(ctx, db.CreateAPIKeyParams{
			UserID: userID, Name: name, KeyHash: hashToken([]byte(key)),
			KeyPrefix: key[:apiKeyPrefixLen], CreatedAt: s.now(),
		})
		if err != nil {
			return fmt.Errorf("creating API key: %w", err)
		}
		return audit(ctx, q, userID, "api_key.create", "api_key", created.ID.String(),
			map[string]any{"name": name, "prefix": created.KeyPrefix})
	})
	if err != nil {
		return NewAPIKey{}, err
	}
	return NewAPIKey{APIKey: toAPIKey(created), Key: key}, nil
}

// ListAPIKeys returns userID's active keys, newest first.
func (s *Service) ListAPIKeys(ctx context.Context, userID uuid.UUID) ([]APIKey, error) {
	rows, err := s.st.ListActiveAPIKeys(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("listing API keys: %w", err)
	}
	out := make([]APIKey, 0, len(rows))
	for _, k := range rows {
		out = append(out, toAPIKey(k))
	}
	return out, nil
}

// RevokeAPIKey revokes one of userID's keys. A key that belongs to someone
// else, or is already revoked, is reported as not found.
func (s *Service) RevokeAPIKey(ctx context.Context, userID, keyID uuid.UUID) error {
	return s.st.InTx(ctx, func(q *db.Queries) error {
		k, err := q.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{ID: keyID, UserID: userID, RevokedAt: s.now()})
		if store.IsNotFound(err) {
			return errKeyNotFound
		}
		if err != nil {
			return fmt.Errorf("revoking API key: %w", err)
		}
		return audit(ctx, q, userID, "api_key.revoke", "api_key", k.ID.String(),
			map[string]any{"name": k.Name, "prefix": k.KeyPrefix})
	})
}

// apiKeyFromRequest extracts a key from, in order: "Authorization: Bearer
// <key>", "Authorization: Basic <base64>" (wakatime-cli sends base64(key);
// base64("key:") and base64(":key") are tolerated), and the api_key query
// parameter. ok is false if none is present or the header is malformed.
func apiKeyFromRequest(r *http.Request) (key string, ok bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, cred, found := strings.Cut(h, " ")
		if !found {
			return "", false
		}
		cred = strings.TrimSpace(cred)
		switch strings.ToLower(scheme) {
		case "bearer":
			return cred, cred != ""
		case "basic":
			raw, err := base64.StdEncoding.DecodeString(cred)
			if err != nil {
				return "", false
			}
			user, pass, hasColon := strings.Cut(string(raw), ":")
			switch {
			case !hasColon:
				return user, user != ""
			case user != "":
				return user, true
			default:
				return pass, pass != ""
			}
		default:
			return "", false
		}
	}
	if k := r.URL.Query().Get("api_key"); k != "" {
		return k, true
	}
	return "", false
}

// authenticateAPIKey resolves a key to its owner.
func (s *Service) authenticateAPIKey(ctx context.Context, key string) (httpapi.CurrentUser, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if !apiKeyRe.MatchString(key) {
		return httpapi.CurrentUser{}, errInvalidAPIKey
	}
	row, err := s.st.GetActiveAPIKeyByHash(ctx, hashToken([]byte(key)))
	if store.IsNotFound(err) {
		return httpapi.CurrentUser{}, errInvalidAPIKey
	}
	if err != nil {
		return httpapi.CurrentUser{}, fmt.Errorf("looking up API key: %w", err)
	}
	now := s.now()
	if row.LastUsedAt == nil || now.Sub(*row.LastUsedAt) >= apiKeyTouchEvery {
		if err := s.st.TouchAPIKey(ctx, db.TouchAPIKeyParams{
			UsedAt: now, ID: row.ID, TouchBefore: now.Add(-apiKeyTouchEvery),
		}); err != nil {
			return httpapi.CurrentUser{}, fmt.Errorf("touching API key: %w", err)
		}
	}
	return httpapi.CurrentUser{ID: row.UserID, Username: row.Username, Role: row.Role, Via: "api_key"}, nil
}

// RequireAPIKey authenticates editor plugins and the agent by API key (see
// apiKeyFromRequest for the accepted forms).
//
// A valid key always gets through: many users share one NAT address, and one
// misconfigured neighbour must not block their heartbeats. Only failures are
// rate limited. Each one takes a token from the client's budget in a single
// atomic step; a failure with a token left gets 401, one without gets 429.
// Successful requests never create or touch a limiter bucket.
func (s *Service) RequireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authenticateRequest(r)
		if err == nil {
			next.ServeHTTP(w, r.WithContext(httpapi.WithUser(r.Context(), u)))
			return
		}
		if errors.Is(err, errInvalidAPIKey) {
			if k := s.limitKey(r); !s.keyFailLimiter.Allow(k) {
				httpapi.WriteTooManyRequests(w, r, s.keyFailLimiter.RetryAfter(k))
				return
			}
		}
		httpapi.WriteError(w, r, err)
	})
}

// authenticateRequest resolves the request's API key, or returns
// errInvalidAPIKey when it has none or it doesn't verify.
func (s *Service) authenticateRequest(r *http.Request) (httpapi.CurrentUser, error) {
	key, ok := apiKeyFromRequest(r)
	if !ok {
		return httpapi.CurrentUser{}, errInvalidAPIKey
	}
	return s.authenticateAPIKey(r.Context(), key)
}

// ErrInvalidAPIKey is returned by APIKeyOwner for a key that is malformed,
// unknown or revoked.
var ErrInvalidAPIKey = errors.New("invalid API key")

// APIKeyOwner returns the user an API key belongs to, for callers that take
// the key from somewhere other than RequireAPIKey's forms (the program API
// reads it from a Bearer or Basic header, as Hackatime does). It returns
// ErrInvalidAPIKey when the key doesn't verify.
func (s *Service) APIKeyOwner(ctx context.Context, key string) (uuid.UUID, error) {
	u, err := s.authenticateAPIKey(ctx, key)
	if errors.Is(err, errInvalidAPIKey) {
		return uuid.Nil, ErrInvalidAPIKey
	}
	if err != nil {
		return uuid.Nil, err
	}
	return u.ID, nil
}
