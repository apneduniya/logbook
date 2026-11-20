// Package config loads the server's configuration from environment variables.
// It is read once in cmd/logbook-server and passed down; nothing else reads env.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config holds every server setting. Secrets are tagged so they never print.
type Config struct {
	Addr        string `env:"LOGBOOK_ADDR" envDefault:"127.0.0.1:8080"`
	BaseURL     string `env:"LOGBOOK_BASE_URL" envDefault:"http://127.0.0.1:8080"`
	DatabaseURL Secret `env:"DATABASE_URL,required"`
	LogLevel    string `env:"LOGBOOK_LOG_LEVEL" envDefault:"info"`

	// WebURL is the web UI's origin (ADR 0011), such as
	// https://logbook.boogles.dev. The API allows CORS from exactly this
	// origin and sends browsers here after sign-in. It is required unless
	// LOGBOOK_DEV_INSECURE_COOKIE is set, when it defaults to DevWebURL.
	WebURL string `env:"LOGBOOK_WEB_URL"`

	// EncryptionKey is a base64-encoded 32-byte key for AES-GCM token encryption.
	EncryptionKey Secret `env:"LOGBOOK_ENCRYPTION_KEY,required"`

	HCA       OAuthClient `envPrefix:"LOGBOOK_HCA_"`
	Hackatime OAuthClient `envPrefix:"LOGBOOK_HACKATIME_"`

	SessionTTL        time.Duration `env:"LOGBOOK_SESSION_TTL" envDefault:"720h"`
	ShutdownTimeout   time.Duration `env:"LOGBOOK_SHUTDOWN_TIMEOUT" envDefault:"15s"`
	DevInsecureCookie bool          `env:"LOGBOOK_DEV_INSECURE_COOKIE" envDefault:"false"`

	// DevLogin enables POST /auth/dev-login for local development. Load
	// refuses it unless BaseURL points at localhost or 127.0.0.1 and Addr
	// listens on loopback only.
	DevLogin bool `env:"LOGBOOK_DEV_LOGIN" envDefault:"false"`

	// TrustProxy makes rate limits key on the last X-Forwarded-For hop, as
	// set by the reverse proxy in front of the server. Enable it only when
	// such a proxy always overwrites or appends that header.
	TrustProxy bool `env:"LOGBOOK_TRUST_PROXY" envDefault:"false"`
}

// DevWebURL is the web UI's origin in development (the SvelteKit dev server).
const DevWebURL = "http://localhost:5173"

// DefaultHCABaseURL is Hack Club Auth's production host.
const DefaultHCABaseURL = "https://auth.hackclub.com"

// OAuthClient is the configuration for one upstream OAuth provider.
type OAuthClient struct {
	BaseURL      string `env:"BASE_URL"`
	ClientID     string `env:"CLIENT_ID"`
	ClientSecret Secret `env:"CLIENT_SECRET"`
}

// Secret is a string that redacts itself when printed or logged.
type Secret string

// String implements fmt.Stringer and hides the value.
func (Secret) String() string { return "[redacted]" }

// LogValue keeps secrets out of slog output.
func (Secret) LogValue() string { return "[redacted]" }

// Reveal returns the underlying value. Call it only where the secret is used.
func (s Secret) Reveal() string { return string(s) }

// Load reads and validates configuration from the environment.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parsing env: %w", err)
	}
	if c.HCA.BaseURL == "" {
		c.HCA.BaseURL = DefaultHCABaseURL
	}
	if c.WebURL == "" && c.DevInsecureCookie {
		c.WebURL = DevWebURL
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// EncryptionKeyBytes decodes the AES-GCM key.
func (c Config) EncryptionKeyBytes() ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(c.EncryptionKey.Reveal())
	if err != nil {
		return nil, fmt.Errorf("decoding LOGBOOK_ENCRYPTION_KEY: %w", err)
	}
	if len(k) != 32 {
		return nil, errors.New("LOGBOOK_ENCRYPTION_KEY must decode to 32 bytes")
	}
	return k, nil
}

func (c Config) validate() error {
	if _, err := c.EncryptionKeyBytes(); err != nil {
		return err
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("LOGBOOK_BASE_URL %q is not an absolute URL", c.BaseURL)
	}
	if u.Scheme != "https" && !c.DevInsecureCookie {
		return errors.New("LOGBOOK_BASE_URL must be https unless LOGBOOK_DEV_INSECURE_COOKIE=true")
	}
	if c.DevLogin && !IsLoopbackHost(u.Hostname()) {
		return errors.New("LOGBOOK_DEV_LOGIN is only allowed when LOGBOOK_BASE_URL is on localhost or 127.0.0.1")
	}
	if c.DevLogin {
		// An empty or wildcard host (":8080", "0.0.0.0:8080") listens on
		// every interface and would expose dev sign-in to the network.
		host, _, err := net.SplitHostPort(c.Addr)
		if err != nil || !IsLoopbackHost(host) {
			return fmt.Errorf("LOGBOOK_DEV_LOGIN requires LOGBOOK_ADDR to listen on loopback only (got %q)", c.Addr)
		}
	}
	if err := c.validateWebURL(); err != nil {
		return err
	}
	hca, err := url.Parse(c.HCA.BaseURL)
	if err != nil || hca.Host == "" || (hca.Scheme != "https" && !IsLoopbackHost(hca.Hostname())) {
		return fmt.Errorf("LOGBOOK_HCA_BASE_URL %q must be an https URL", c.HCA.BaseURL)
	}
	return nil
}

// validateWebURL accepts an absolute https origin with nothing after the
// host, or plain http on a loopback host in development only. A path,
// query, fragment or user info would make "WebURL + next path" ambiguous.
func (c Config) validateWebURL() error {
	if c.WebURL == "" {
		return errors.New("LOGBOOK_WEB_URL is required (the web UI's origin, e.g. https://logbook.boogles.dev)")
	}
	origin, err := ParseOrigin(c.WebURL)
	if err != nil {
		return fmt.Errorf("LOGBOOK_WEB_URL: %w", err)
	}
	u, _ := url.Parse(origin) // ParseOrigin has parsed it already
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && IsLoopbackHost(u.Hostname()) && c.DevInsecureCookie:
	default:
		return fmt.Errorf("LOGBOOK_WEB_URL %q must be https (plain http only on localhost with LOGBOOK_DEV_INSECURE_COOKIE=true)", c.WebURL)
	}
	return nil
}

// WebOrigin returns WebURL as a browser origin (see ParseOrigin). It is
// what browsers send in Origin, and what CORS and sign-in redirects use.
func (c Config) WebOrigin() string {
	origin, err := ParseOrigin(c.WebURL)
	if err != nil {
		return "" // Load has validated WebURL; an empty origin allows nothing
	}
	return origin
}

// ParseOrigin turns raw, an http or https URL with nothing after the host
// (a trailing "/" is allowed), into the origin a browser sends in its
// Origin header: lower-case scheme://host[:port]. A path, query, fragment
// or user info would make "origin + path" ambiguous, so they are errors.
// So is an explicit default port (":443" for https, ":80" for http) or an
// empty one: browsers leave those out of Origin, so the origin would never
// match.
func ParseOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Opaque != "" ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("%q must be an origin like https://logbook.boogles.dev", raw)
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") ||
		(u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return "", fmt.Errorf("%q must leave out the default port, as browsers do in Origin", raw)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// IsLoopbackHost reports whether host (without port) names this machine.
func IsLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
