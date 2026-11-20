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
	// BaseURL is the API's public origin, e.g. https://api.logbook.boogles.dev.
	BaseURL string
	// WebURL is the web UI's origin, e.g. https://logbook.boogles.dev (ADR
	// 0011). After sign-in the browser is sent to WebURL plus a safe "next"
	// path. Empty means the UI shares the API's origin, so redirects are
	// plain paths.
	WebURL string
	HCA    HCAConfig
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
	webOrigin  string // "" when the UI shares the API's origin
	hca        *hcaClient
	stateKey   []byte
	sessionTTL time.Duration
	secure     bool
	devLogin   bool
	trustProxy bool
	now        func() time.Time

	// Per-client limiters (see rateLimit* below). Budgets are sized for
	// shared NATs: a school or hackathon venue is often one IPv4 address.
	signInLimiter  *httpapi.RateLimiter // GET /auth/login, POST /auth/dev-login
	authLimiter    *httpapi.RateLimiter // GET /auth/callback, POST /auth/logout
	keyFailLimiter *httpapi.RateLimiter // failed API key attempts only
}

// Rate-limit budgets per client (IPv4 address or IPv6 /64): a burst of N,
// refilled at N per minute.
const (
	// signInBurst covers sign-in starts. Each costs one request, and the
	// callback is limited separately, so a 100-person NAT can all sign in at
	// once.
	signInBurst = 120
	// authBurst covers the callback and logout. It is looser than
	// signInBurst because a 429 on the callback throws away a code the user
	// just approved, and the state cookie already gates it.
	authBurst = 300
	// keyFailBurst counts only failed API key attempts; valid keys never
	// touch it. Keys carry 122 random bits, so this limit is about slowing
	// misbehaving clients and bounding wasted lookups, not brute force.
	keyFailBurst = 60

	limiterMaxKeys = 10_000
	limiterIdleTTL = 10 * time.Minute
)

func perMinuteLimiter(burst int) *httpapi.RateLimiter {
	return httpapi.NewRateLimiter(float64(burst)/60, burst, limiterMaxKeys, limiterIdleTTL)
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
	webOrigin, err := originOf(opts.WebURL)
	if err != nil {
		return nil, err
	}
	hca := newHCAClient(opts.HCA, base.String()+"/auth/callback", client)
	s := &Service{
		st:         st,
		baseURL:    base,
		webOrigin:  webOrigin,
		hca:        hca,
		stateKey:   stateKey,
		sessionTTL: ttl,
		secure:     !opts.InsecureCookies,
		// Defence in depth: config validation already refuses this combination.
		devLogin:       opts.DevLogin && config.IsLoopbackHost(base.Hostname()),
		trustProxy:     opts.TrustProxy,
		now:            func() time.Time { return now().UTC() },
		signInLimiter:  perMinuteLimiter(signInBurst),
		authLimiter:    perMinuteLimiter(authBurst),
		keyFailLimiter: perMinuteLimiter(keyFailBurst),
	}
	if opts.DevLogin && !s.devLogin {
		slog.Warn("dev login requested but base URL is not loopback; leaving it disabled")
	}
	return s, nil
}

// limitKey is the rate-limit bucket for the request's client.
func (s *Service) limitKey(r *http.Request) string {
	return httpapi.LimitKey(httpapi.ClientIP(r, s.trustProxy))
}

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
