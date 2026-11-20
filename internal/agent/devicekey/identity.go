package devicekey

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
	// KeyPrevID is the last device id that was cleared (the server forgot
	// it, or the key was retired), so the next registration knows whether
	// it replaced a device.
	KeyPrevID = "device.prev_id"
	// KeyGoodKey is a fingerprint of the API key the server last accepted
	// for the current device id (at registration or on an upload).
	KeyGoodKey = "device.good_key"
)

// DefaultName is the device name used when the host name is empty or not
// a valid label.
const DefaultName = "Logbook device"

// Server limits on device labels (POST /api/v1/devices), in characters.
const (
	maxNameChars = 64
	maxOSChars   = 32
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
	// Name and OS describe the device to the user's device list. An empty
	// or invalid Name becomes DefaultName.
	Name, OS string
	Now      func() time.Time
	Logger   *slog.Logger
}

// Device is a registered device.
type Device struct {
	ID           string
	RegisteredAt time.Time
	Key          *Key
	// Replaced is true when this call registered a new device in place of
	// an earlier one (a new id or a new key): a new chain begins, and
	// minutes from before it registered are not sent under it.
	Replaced bool
}

// Identity keeps the device registered. It is safe for concurrent use.
type Identity struct {
	opt Options

	mu   sync.Mutex
	key  *Key
	good string // cached KeyGoodKey
}

// NewIdentity returns an Identity.
func NewIdentity(o Options) *Identity {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	o.Name = cleanLabel(o.Name, maxNameChars)
	if o.Name == "" {
		o.Name = DefaultName
	}
	o.OS = cleanLabel(o.OS, maxOSChars)
	if o.OS == "" {
		o.OS = "unknown"
	}
	return &Identity{opt: o}
}

// cleanLabel trims s, drops characters the server refuses in labels
// (control, bidirectional-control and replacement characters) and keeps at
// most n characters.
func cleanLabel(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > n {
		s = strings.TrimSpace(string([]rune(s)[:n]))
	}
	return s
}

// Ensure returns the device registered with t's server, registering it
// first if needed. It returns ErrRevoked while the device is revoked and the
// API key hasn't changed, and a *serverapi.Error when registration fails.
// Only the server's "public key is registered to another account" answer
// makes it replace the key; any other refusal (such as the active-device
// limit) is returned as is.
func (i *Identity) Ensure(ctx context.Context, t serverapi.Target) (Device, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	st, err := i.opt.Store.Prefix(ctx, "device.")
	if err != nil {
		return Device{}, err
	}
	oldID := cmp.Or(st[KeyID], st[KeyPrevID])
	rotated := false
	if revoked := st[KeyRevokedKey]; revoked != "" {
		if revoked == fingerprint(t.APIKey) {
			return Device{}, ErrRevoked
		}
		// A new API key: pair this machine again as a new device.
		i.opt.Logger.InfoContext(ctx, "API key changed after revocation; registering a new device")
		if err := i.rotate(ctx, oldID); err != nil {
			return Device{}, err
		}
		st[KeyID], rotated = "", true // the old id must not be reused
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
	if serverapi.IsKeyTaken(err) {
		// The key is registered to another account (the user signed in with
		// a different one). Its evidence stays with that account; this
		// account gets a fresh key.
		i.opt.Logger.WarnContext(ctx, "device key belongs to another account; generating a new one")
		if err := i.rotate(ctx, oldID); err != nil {
			return Device{}, err
		}
		rotated = true
		if key, err = i.loadKey(); err != nil {
			return Device{}, err
		}
		id, err = i.register(ctx, t, key)
	}
	if err != nil {
		return Device{}, err
	}
	now := i.opt.Now().UTC().Truncate(time.Second)
	good := fingerprint(t.APIKey)
	if err := i.opt.Store.SetMany(ctx,
		localdb.KV{Key: KeyID, Value: id}, localdb.KV{Key: KeyServer, Value: t.BaseURL},
		localdb.KV{Key: KeyRegisteredAt, Value: now.Format(time.RFC3339)},
		localdb.KV{Key: KeyRevokedKey, Value: ""}, localdb.KV{Key: KeyRevokedAt, Value: ""},
		localdb.KV{Key: KeyPrevID, Value: ""}, localdb.KV{Key: KeyGoodKey, Value: good}); err != nil {
		return Device{}, err
	}
	i.good = good
	i.opt.Logger.InfoContext(ctx, "device registered", "device_id", id)
	return Device{ID: id, RegisteredAt: now, Key: key, Replaced: rotated || oldID != "" && oldID != id}, nil
}

// Confirm records that the server accepted an upload for the current device
// with t's API key. MarkRevoked uses it to tell a revocation of this key's
// device from one the user already answered with `logbook login`.
func (i *Identity) Confirm(ctx context.Context, t serverapi.Target) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	fp := fingerprint(t.APIKey)
	if i.good == fp {
		return nil
	}
	if err := i.opt.Store.SetMany(ctx, localdb.KV{Key: KeyGoodKey, Value: fp}); err != nil {
		return err
	}
	i.good = fp
	return nil
}

// MarkRevoked handles the server saying the device is revoked while t's API
// key is in use. If the server never accepted that key for this device, the
// user revoked it and already ran `logbook login` since: the key is retired
// and repaired is true, so the next Ensure registers a new device.
// Otherwise the revocation is recorded and Ensure returns ErrRevoked until
// the API key changes.
func (i *Identity) MarkRevoked(ctx context.Context, t serverapi.Target) (repaired bool, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	st, err := i.opt.Store.Prefix(ctx, "device.")
	if err != nil {
		return false, err
	}
	fp := fingerprint(t.APIKey)
	if good := st[KeyGoodKey]; good != "" && good != fp {
		i.opt.Logger.InfoContext(ctx, "device revoked before the current API key was used; registering a new device")
		return true, i.rotate(ctx, cmp.Or(st[KeyID], st[KeyPrevID]))
	}
	return false, i.opt.Store.SetMany(ctx,
		localdb.KV{Key: KeyRevokedKey, Value: fp},
		localdb.KV{Key: KeyRevokedAt, Value: i.opt.Now().UTC().Format(time.RFC3339)})
}

// Forget clears the stored device id, so the next Ensure registers the
// same key again (the server said it doesn't know the device). The id is
// kept as the previous id, so a different id on re-registration counts as
// a replacement.
func (i *Identity) Forget(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	st, err := i.opt.Store.Prefix(ctx, "device.")
	if err != nil {
		return err
	}
	return i.opt.Store.SetMany(ctx, localdb.KV{Key: KeyID, Value: ""},
		localdb.KV{Key: KeyPrevID, Value: cmp.Or(st[KeyID], st[KeyPrevID])})
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

// rotate retires the key and clears the device state at once, so a failed
// registration afterwards never retires the fresh key again. prevID is
// remembered so the next registration counts as a replacement.
func (i *Identity) rotate(ctx context.Context, prevID string) error {
	i.key = nil
	if err := Retire(i.opt.KeyPath, i.opt.Now()); err != nil {
		return err
	}
	i.good = ""
	return i.opt.Store.SetMany(ctx, localdb.KV{Key: KeyID, Value: ""}, localdb.KV{Key: KeyPrevID, Value: prevID},
		localdb.KV{Key: KeyRevokedKey, Value: ""}, localdb.KV{Key: KeyRevokedAt, Value: ""},
		localdb.KV{Key: KeyGoodKey, Value: ""})
}

// fingerprint identifies an API key without storing it (for KeyRevokedKey
// and KeyGoodKey). The label is historical and kept so stored values match.
func fingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte("logbook-device-revoked\x00" + apiKey))
	return hex.EncodeToString(sum[:16])
}
