package auth

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/config"
	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Store is the database access auth needs. *store.Store satisfies it.
type Store interface {
	db.Querier
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
}

// HCAConfig points at a Hack Club Auth deployment and our OAuth app on it.
type HCAConfig struct {
	BaseURL      string // e.g. https://auth.hackclub.com
	ClientID     string
	ClientSecret string
}

// Options configures a Service. Zero durations take the defaults below.
type Options struct {
	// BaseURL is Logbook's public origin, e.g. https://logbook.hackclub.com.
	BaseURL string
	HCA     HCAConfig
	// SecretKey is the 32-byte server encryption key. Auth derives its own
	// keys from it with HKDF and never uses it directly.
	SecretKey []byte
	// SessionTTL is the sliding session lifetime.
	SessionTTL time.Duration
	// InsecureCookies drops the Secure cookie flag (plain-http development).
	InsecureCookies bool
	// DevLogin enables POST /auth/dev-login. It is ignored unless BaseURL is
	// on localhost or 127.0.0.1.
	DevLogin bool
	// TrustProxy keys rate limits on the proxy-set X-Forwarded-For hop.
	TrustProxy bool
	// HTTPClient is used for calls to HCA. Defaults to a client with a timeout.
	HTTPClient *http.Client
	// Now overrides the clock (tests).
	Now func() time.Time
}

const (
	defaultSessionTTL = 30 * 24 * time.Hour
	// sessionTouchEvery throttles last_seen_at/expiry writes per session.
	sessionTouchEvery = 5 * time.Minute
	// apiKeyTouchEvery throttles last_used_at writes per API key.
	apiKeyTouchEvery = time.Minute
	// outboundTimeout bounds every call to HCA.
	outboundTimeout = 10 * time.Second
	// MaxActiveAPIKeys caps unrevoked keys per user.
	MaxActiveAPIKeys = 20
)

// Service implements sign-in, sessions and API keys.
type Service struct {
	st         Store
	baseURL    *url.URL
	hca        *hcaClient
	stateKey   []byte
	sessionTTL time.Duration
	secure     bool
	devLogin   bool
	trustProxy bool
	now        func() time.Time

	// authLimiter limits /auth/* per IP; keyFailLimiter limits failed API
	// key attempts per IP.
	authLimiter    *httpapi.RateLimiter
	keyFailLimiter *httpapi.RateLimiter
}

// New builds a Service. It fails on configuration that would be unsafe.
func New(st Store, opts Options) (*Service, error) {
	base, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("auth: base URL %q is not absolute", opts.BaseURL)
	}
	if len(opts.SecretKey) < 32 {
		return nil, errors.New("auth: secret key must be at least 32 bytes")
	}
	stateKey, err := hkdf.Key(sha256.New, opts.SecretKey, nil, "logbook/auth/oauth-state/v1", 32)
	if err != nil {
		return nil, fmt.Errorf("auth: deriving state key: %w", err)
	}
	ttl := opts.SessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: outboundTimeout}
	}
	hca := newHCAClient(opts.HCA, base.String()+"/auth/callback", client)
	s := &Service{
		st:         st,
		baseURL:    base,
		hca:        hca,
		stateKey:   stateKey,
		sessionTTL: ttl,
		secure:     !opts.InsecureCookies,
		// Defence in depth: config validation already refuses this combination.
		devLogin:       opts.DevLogin && config.IsLoopbackHost(base.Hostname()),
		trustProxy:     opts.TrustProxy,
		now:            func() time.Time { return now().UTC() },
		authLimiter:    httpapi.NewRateLimiter(20.0/60, 20, 10_000, 10*time.Minute),
		keyFailLimiter: httpapi.NewRateLimiter(10.0/60, 10, 10_000, 10*time.Minute),
	}
	if opts.DevLogin && !s.devLogin {
		slog.Warn("dev login requested but base URL is not loopback; leaving it disabled")
	}
	return s, nil
}

func (s *Service) clientIP(r *http.Request) string { return httpapi.ClientIP(r, s.trustProxy) }

// audit writes one audit_log row. data must be JSON-encodable and must never
// contain secrets.
func audit(ctx context.Context, q db.Querier, actor uuid.UUID, action, subjectType, subjectID string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorUserID: pgtype.UUID{Bytes: actor, Valid: true},
		Action:      action,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		Data:        b,
	}); err != nil {
		return fmt.Errorf("writing audit log %s: %w", action, err)
	}
	return nil
}
