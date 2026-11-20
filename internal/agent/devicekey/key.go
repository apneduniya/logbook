package devicekey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/apneduniya/logbook/internal/agent/install/atomicfile"
	"github.com/apneduniya/logbook/pkg/evidence"
)

const pemType = "PRIVATE KEY"

// ErrNoKey means the key file doesn't exist.
var ErrNoKey = errors.New("no device key")

// Key is the device's ed25519 signing key. Its zero value is unusable.
type Key struct {
	priv ed25519.PrivateKey
}

// Public returns the public key.
func (k *Key) Public() ed25519.PublicKey {
	return k.priv.Public().(ed25519.PublicKey) //nolint:forcetypeassert // ed25519.PrivateKey.Public always returns ed25519.PublicKey
}

// Sign signs an evidence day's chain hash.
func (k *Key) Sign(d evidence.Day) (chainHash string, sig []byte, err error) {
	return evidence.Sign(k.priv, d)
}

// String keeps the private key out of formatted output.
func (k *Key) String() string { return "devicekey.Key(redacted)" }

// GoString keeps the private key out of %#v.
func (k *Key) GoString() string { return k.String() }

// LogValue keeps the private key out of structured logs.
func (k *Key) LogValue() slog.Value { return slog.StringValue(k.String()) }

// Load reads the key at path. A missing file is ErrNoKey. On Unix, a file
// readable by others is tightened to 0600.
func Load(path string) (*Key, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is built from the agent's data dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, fmt.Errorf("reading device key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != pemType {
		return nil, fmt.Errorf("device key %s is not a PEM %q block", path, pemType)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing device key: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("device key %s is not an ed25519 key", path)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(path, 0o600); err != nil {
				return nil, fmt.Errorf("restricting device key permissions: %w", err)
			}
		}
	}
	return &Key{priv: priv}, nil
}

// Create generates a new key and writes it to path with mode 0600. It fails
// if path already exists, so an existing identity is never overwritten.
func Create(path string) (*Key, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("device key %s already exists", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("checking device key: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating device key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("encoding device key: %w", err)
	}
	if err := atomicfile.Write(path, pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der}), 0o600); err != nil {
		return nil, fmt.Errorf("saving device key: %w", err)
	}
	return &Key{priv: priv}, nil
}

// LoadOrCreate loads the key at path, creating it if missing. created
// reports whether a new key was generated.
func LoadOrCreate(path string) (k *Key, created bool, err error) {
	k, err = Load(path)
	if errors.Is(err, ErrNoKey) {
		k, err = Create(path)
		return k, err == nil, err
	}
	return k, false, err
}

// maxRetiredNames bounds the search for a free retired-key name.
const maxRetiredNames = 1000

// Retire renames the key at path to path.retired-<unix seconds>[-n], so the
// next LoadOrCreate makes a new one. The old key is kept, not deleted: it
// signed evidence the server still holds. The name is reserved with an
// exclusive create first, so a rename never replaces an earlier retired key
// (os.Rename overwrites on every platform).
func Retire(path string, now time.Time) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	base := path + ".retired-" + strconv.FormatInt(now.Unix(), 10)
	for n := range maxRetiredNames {
		dst := base
		if n > 0 {
			dst += "-" + strconv.Itoa(n)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is built from the agent's data dir
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reserving retired key name: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("reserving retired key name: %w", err)
		}
		if err := os.Rename(path, dst); err != nil {
			// The placeholder is empty and ours; the rename error is the one
			// that matters.
			_ = os.Remove(dst)
			return fmt.Errorf("retiring device key: %w", err)
		}
		return nil
	}
	return fmt.Errorf("retiring device key: no free name after %d tries", maxRetiredNames)
}
