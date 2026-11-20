// Package config loads the server's configuration from environment variables.
// It is read once in cmd/logbook-server and passed down; nothing else reads env.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config holds every server setting. Secrets are tagged so they never print.
type Config struct {
	Addr        string `env:"LOGBOOK_ADDR" envDefault:"127.0.0.1:8080"`
	BaseURL     string `env:"LOGBOOK_BASE_URL" envDefault:"http://127.0.0.1:8080"`
	DatabaseURL Secret `env:"DATABASE_URL,required"`
	LogLevel    string `env:"LOGBOOK_LOG_LEVEL" envDefault:"info"`

	// EncryptionKey is a base64-encoded 32-byte key for AES-GCM token encryption.
	EncryptionKey Secret `env:"LOGBOOK_ENCRYPTION_KEY,required"`

	HCA       OAuthClient `envPrefix:"LOGBOOK_HCA_"`
	Hackatime OAuthClient `envPrefix:"LOGBOOK_HACKATIME_"`

	SessionTTL        time.Duration `env:"LOGBOOK_SESSION_TTL" envDefault:"720h"`
	ShutdownTimeout   time.Duration `env:"LOGBOOK_SHUTDOWN_TIMEOUT" envDefault:"15s"`
	DevInsecureCookie bool          `env:"LOGBOOK_DEV_INSECURE_COOKIE" envDefault:"false"`
}

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
	return nil
}
