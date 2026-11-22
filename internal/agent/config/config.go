// Package config owns the agent's on-disk configuration and data directory
// (ADR 0009). Every agent package and hook command finds its files through
// here, so the layout is defined in one place.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
)

// DefaultLocalAddr is where the daemon's local HTTP server listens.
const DefaultLocalAddr = "127.0.0.1:47231"

// DefaultServerURL is the Logbook server the agent talks to.
const DefaultServerURL = "https://api.logbook.boogles.dev"

// Config is the agent's settings, stored as JSON with mode 0600.
type Config struct {
	// ServerURL is the Logbook server base URL.
	ServerURL string `json:"server_url"`
	// APIKey is the user's Logbook API key (also written into .wakatime.cfg).
	APIKey string `json:"api_key"`
	// LocalAddr is the daemon's loopback listen address.
	LocalAddr string `json:"local_addr"`
	// LocalToken authenticates hook commands to the daemon (X-Logbook-Token).
	LocalToken string `json:"local_token"`
	// Hackatime holds the user's original Hackatime settings, used for mirroring.
	Hackatime Hackatime `json:"hackatime"`
}

// Hackatime is the mirror target (ADR 0005). The key never leaves the device
// except to Hackatime itself.
type Hackatime struct {
	URL    string `json:"url,omitempty"`
	APIKey string `json:"api_key,omitempty"`
	Mirror bool   `json:"mirror"`
}

// Dir returns the agent's data directory, creating it with mode 0700.
// LOGBOOK_HOME overrides it (used by tests and portable installs).
func Dir() (string, error) {
	dir := os.Getenv("LOGBOOK_HOME")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("finding config dir: %w", err)
		}
		dir = filepath.Join(base, "logbook")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // dir is the user's own config dir or LOGBOOK_HOME, set by the user
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	return dir, nil
}

// Path returns the config file path inside dir.
func Path(dir string) string { return filepath.Join(dir, "config.json") }

// DBPath returns the local SQLite database path inside dir.
func DBPath(dir string) string { return filepath.Join(dir, "logbook.db") }

// SpoolPath returns the hook spool file used when the daemon is down.
func SpoolPath(dir string) string { return filepath.Join(dir, "spool", "events.jsonl") }

// LogPath returns the daemon log file path inside dir.
func LogPath(dir string) string { return filepath.Join(dir, "agent.log") }

// DeviceKeyPath returns the device's ed25519 private key file inside dir.
func DeviceKeyPath(dir string) string { return filepath.Join(dir, "device.key") }

// Default returns a fresh config with a new local token.
func Default() (Config, error) {
	tok, err := NewToken()
	if err != nil {
		return Config{}, err
	}
	return Config{ServerURL: DefaultServerURL, LocalAddr: DefaultLocalAddr, LocalToken: tok,
		Hackatime: Hackatime{Mirror: true}}, nil
}

// NewToken returns 32 random bytes, hex-encoded.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ErrNotConfigured means no config file exists yet (run `logbook install`).
var ErrNotConfigured = errors.New("logbook is not configured; run `logbook install`")

// Load reads the config from dir.
func Load(dir string) (Config, error) {
	b, err := os.ReadFile(Path(dir)) //nolint:gosec // path is built from our own data dir
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, ErrNotConfigured
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save writes the config atomically with mode 0600.
func Save(dir string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ") //nolint:gosec // the config file is meant to hold the keys; it is written 0600
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "config-*.json")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod config: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing config: %w", err)
	}
	if err := os.Rename(tmp.Name(), Path(dir)); err != nil {
		return fmt.Errorf("replacing config: %w", err)
	}
	return nil
}

// Validate checks the config's fields.
func (c Config) Validate() error {
	if u, err := url.Parse(c.ServerURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("server_url %q is not a valid URL", c.ServerURL)
	}
	if len(c.LocalToken) < 32 {
		return errors.New("local_token is missing or too short")
	}
	if c.LocalAddr == "" {
		return errors.New("local_addr is required")
	}
	return nil
}
