package devicekey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
)

// Key-value state, under "device.".
const (
	KeyID           = "device.id"
	KeyServer       = "device.server"
	KeyRegisteredAt = "device.registered_at"
	// KeyRevokedKey holds a fingerprint of the API key in use when the
	// server revoked the device; KeyRevokedAt when that happened.
	KeyRevokedKey = "device.revoked_key"
	KeyRevokedAt  = "device.revoked_at"
)

// ErrRevoked means the server revoked this device. Nothing is uploaded
// until the API key changes.
var ErrRevoked = errors.New("this device was revoked on the Logbook server; run `logbook login` with a new API key to pair it again")

// Store is the key-value state Identity needs. *localdb.DB satisfies it.
type Store interface {
	Prefix(ctx context.Context, prefix string) (map[string]string, error)
	SetMany(ctx context.Context, kvs ...localdb.KV) error
}

// Registrar registers a public key. *serverapi.Client satisfies it.
type Registrar interface {
	RegisterDevice(ctx context.Context, t serverapi.Target, d serverapi.DeviceRequest) (string, error)
}

// Options configures an Identity.
type Options struct {
	// KeyPath is the key file (config.DeviceKeyPath).
	KeyPath   string
	Store     Store
	Registrar Registrar
	// Name and OS describe the device to the user's device list.
	Name, OS string
	Now      func() time.Time
	Logger   *slog.Logger
}

// Device is a registered device.
type Device struct {
	ID           string
	RegisteredAt time.Time
	Key          *Key
	// Replaced is true when this call registered a new device id in place
	// of an earlier one (a new chain begins).
	Replaced bool
}

// Identity keeps the device registered. It is safe for concurrent use.
type Identity struct {
	opt Options

	mu  sync.Mutex
	key *Key
}

// NewIdentity returns an Identity.
func NewIdentity(o Options) *Identity {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if len(o.Name) > 64 {
		o.Name = strings.ToValidUTF8(o.Name[:64], "")
	}
	if len(o.OS) > 32 {
		o.OS = o.OS[:32]
	}
	return &Identity{opt: o}
}

// Ensure returns the device registered with t's server, registering it
// first if needed. It returns ErrRevoked while the device is revoked and the
// API key hasn't changed, and a *serverapi.Error when registration fails.
func (i *Identity) Ensure(ctx context.Context, t serverapi.Target) (Device, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	st, err := i.opt.Store.Prefix(ctx, "device.")
	if err != nil {
		return Device{}, err
	}
	oldID := st[KeyID]
	if revoked := st[KeyRevokedKey]; revoked != "" {
		if revoked == fingerprint(t.APIKey) {
			return Device{}, ErrRevoked
		}
		// A new API key: pair this machine again as a new device.
		i.opt.Logger.InfoContext(ctx, "API key changed after revocation; registering a new device")
		if err := i.retireKey(); err != nil {
			return Device{}, err
		}
		st[KeyID] = "" // the old id must not be reused
	}
	key, err := i.loadKey()
	if err != nil {
		return Device{}, err
	}
	if id := st[KeyID]; id != "" && st[KeyServer] == t.BaseURL {
		at, _ := time.Parse(time.RFC3339, st[KeyRegisteredAt]) // zero if missing; only informative
		return Device{ID: id, RegisteredAt: at, Key: key}, nil
	}

	id, err := i.register(ctx, t, key)
	if serverapi.KindOf(err) == serverapi.KindConflict {
		// The key is registered to another account (the user signed in with
		// a different one). Its evidence stays with that account; this
		// account gets a fresh key.
		i.opt.Logger.WarnContext(ctx, "device key belongs to another account; generating a new one")
		if err := i.retireKey(); err != nil {
			return Device{}, err
		}
		if key, err = i.loadKey(); err != nil {
			return Device{}, err
		}
		id, err = i.register(ctx, t, key)
	}
	if err != nil {
		return Device{}, err
	}
	now := i.opt.Now().UTC().Truncate(time.Second)
	if err := i.opt.Store.SetMany(ctx,
		localdb.KV{Key: KeyID, Value: id}, localdb.KV{Key: KeyServer, Value: t.BaseURL},
		localdb.KV{Key: KeyRegisteredAt, Value: now.Format(time.RFC3339)},
		localdb.KV{Key: KeyRevokedKey, Value: ""}, localdb.KV{Key: KeyRevokedAt, Value: ""}); err != nil {
		return Device{}, err
	}
	i.opt.Logger.InfoContext(ctx, "device registered", "device_id", id)
	return Device{ID: id, RegisteredAt: now, Key: key, Replaced: oldID != "" && oldID != id}, nil
}

// MarkRevoked records that the server revoked the device while t's API key
// was in use. Ensure returns ErrRevoked until the key changes.
func (i *Identity) MarkRevoked(ctx context.Context, t serverapi.Target) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.opt.Store.SetMany(ctx,
		localdb.KV{Key: KeyRevokedKey, Value: fingerprint(t.APIKey)},
		localdb.KV{Key: KeyRevokedAt, Value: i.opt.Now().UTC().Format(time.RFC3339)})
}

// Forget clears the stored device id, so the next Ensure registers the
// same key again (the server said it doesn't know the device).
func (i *Identity) Forget(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.opt.Store.SetMany(ctx, localdb.KV{Key: KeyID, Value: ""})
}

func (i *Identity) register(ctx context.Context, t serverapi.Target, key *Key) (string, error) {
	id, err := i.opt.Registrar.RegisterDevice(ctx, t, serverapi.DeviceRequest{
		Name: i.opt.Name, OS: i.opt.OS, PublicKey: key.Public(),
	})
	if err != nil {
		return "", fmt.Errorf("registering device: %w", err)
	}
	return id, nil
}

// loadKey returns the cached key, loading or creating it on first use.
func (i *Identity) loadKey() (*Key, error) {
	if i.key != nil {
		return i.key, nil
	}
	k, created, err := LoadOrCreate(i.opt.KeyPath)
	if err != nil {
		return nil, err
	}
	if created {
		i.opt.Logger.Info("generated a new device key")
	}
	i.key = k
	return k, nil
}

func (i *Identity) retireKey() error {
	i.key = nil
	return Retire(i.opt.KeyPath, i.opt.Now())
}

// fingerprint identifies an API key without storing it.
func fingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte("logbook-device-revoked\x00" + apiKey))
	return hex.EncodeToString(sum[:16])
}
