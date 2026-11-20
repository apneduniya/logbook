package devices

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Limits on registration fields and devices per user.
const (
	MaxNameLen       = 64
	MaxOSLen         = 32
	MaxActiveDevices = 20
)

// Errors callers map to HTTP statuses.
var (
	// ErrInvalid wraps every validation failure; its message is safe to show.
	ErrInvalid = errors.New("invalid device")
	// ErrKeyTaken means the public key is registered to another user.
	ErrKeyTaken = errors.New("public key is registered to another account")
	// ErrTooMany means the user already has MaxActiveDevices active devices.
	ErrTooMany = errors.New("too many active devices")
	// ErrNotFound means the user has no such active device.
	ErrNotFound = errors.New("device not found")
)

// Store is the database access the service needs. *store.Store satisfies it.
type Store interface {
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
	DeviceList(ctx context.Context, userID uuid.UUID) ([]db.Device, error)
}

// Service registers, lists and revokes devices.
type Service struct {
	st  Store
	now func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService returns a Service backed by st.
func NewService(st Store, opts ...Option) *Service {
	s := &Service{st: st, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Registration is a validated registration request.
type Registration struct {
	Name      string
	OS        string
	PublicKey ed25519.PublicKey
}

// ParseRegistration validates the raw request fields. publicKey is standard
// base64 of the 32-byte key.
func ParseRegistration(name, os, publicKey string) (Registration, error) {
	n, ok := cleanLabel(name, MaxNameLen)
	if !ok {
		return Registration{}, fmt.Errorf("%w: name must be 1-%d characters with no control characters", ErrInvalid, MaxNameLen)
	}
	o, ok := cleanLabel(os, MaxOSLen)
	if !ok {
		return Registration{}, fmt.Errorf("%w: os must be 1-%d characters with no control characters", ErrInvalid, MaxOSLen)
	}
	key, err := base64.StdEncoding.Strict().DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Registration{}, fmt.Errorf("%w: public_key must be standard base64 of a 32-byte ed25519 key", ErrInvalid)
	}
	return Registration{Name: n, OS: o, PublicKey: key}, nil
}

// cleanLabel trims s and checks it is 1..maxLen characters of valid UTF-8
// with no control or bidirectional-override characters.
func cleanLabel(s string, maxLen int) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxLen {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == unicode.ReplacementChar {
			return "", false
		}
	}
	return s, true
}

// Registered is the outcome of Register.
type Registered struct {
	ID uuid.UUID
	// Created is false when the key was already registered to this user.
	Created bool
}

// Register adds a device for userID, or returns the existing one when this
// user already registered the key.
func (s *Service) Register(ctx context.Context, userID uuid.UUID, reg Registration) (Registered, error) {
	var out Registered
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		// Serialise registrations per user so the device cap can't be raced.
		if err := q.LockUser(ctx, userID); err != nil {
			return fmt.Errorf("locking user: %w", err)
		}
		existing, err := q.DeviceByPublicKey(ctx, reg.PublicKey)
		switch {
		case err == nil:
			return sameOwner(existing, userID, &out)
		case !store.IsNotFound(err):
			return fmt.Errorf("looking up device key: %w", err)
		}
		n, err := q.DeviceCountActive(ctx, userID)
		if err != nil {
			return fmt.Errorf("counting devices: %w", err)
		}
		if n >= MaxActiveDevices {
			return ErrTooMany
		}
		d, err := q.DeviceInsert(ctx, db.DeviceInsertParams{
			UserID: userID, Name: reg.Name, Os: reg.OS, PublicKey: reg.PublicKey, CreatedAt: s.now().UTC(),
		})
		if store.IsNotFound(err) {
			// Another user registered the key between our read and insert.
			existing, err := q.DeviceByPublicKey(ctx, reg.PublicKey)
			if err != nil {
				return fmt.Errorf("reading raced device key: %w", err)
			}
			return sameOwner(existing, userID, &out)
		}
		if err != nil {
			return fmt.Errorf("inserting device: %w", err)
		}
		out = Registered{ID: d.ID, Created: true}
		return audit(ctx, q, userID, "device.register", d.ID, map[string]any{
			"name": d.Name, "os": d.Os, "key_fingerprint": Fingerprint(d.PublicKey),
		})
	})
	if err != nil {
		return Registered{}, err
	}
	return out, nil
}

func sameOwner(d db.Device, userID uuid.UUID, out *Registered) error {
	if d.UserID != userID {
		return ErrKeyTaken
	}
	*out = Registered{ID: d.ID}
	return nil
}

// Device is a device as shown to its owner.
type Device struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	OS             string     `json:"os"`
	PublicKey      string     `json:"public_key"`
	KeyFingerprint string     `json:"key_fingerprint"`
	CreatedAt      time.Time  `json:"created_at"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
}

// Fingerprint is a short, stable identifier for a public key: the first 16
// hex characters of its SHA-256. The agent shows the same value, so users
// can match a listed device to a machine.
func Fingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:8])
}

func toDevice(d db.Device) Device {
	return Device{
		ID: d.ID, Name: d.Name, OS: d.Os,
		PublicKey:      base64.StdEncoding.EncodeToString(d.PublicKey),
		KeyFingerprint: Fingerprint(d.PublicKey),
		CreatedAt:      d.CreatedAt, LastSeenAt: d.LastSeenAt, RevokedAt: d.RevokedAt,
	}
}

// List returns all of userID's devices, revoked ones included, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	rows, err := s.st.DeviceList(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}
	out := make([]Device, len(rows))
	for i, d := range rows {
		out[i] = toDevice(d)
	}
	return out, nil
}

// Revoke revokes one of userID's active devices. Unknown devices, other
// users' devices and already revoked ones are all ErrNotFound.
func (s *Service) Revoke(ctx context.Context, userID, deviceID uuid.UUID) error {
	return s.st.InTx(ctx, func(q *db.Queries) error {
		d, err := q.DeviceRevoke(ctx, db.DeviceRevokeParams{RevokedAt: s.now().UTC(), ID: deviceID, UserID: userID})
		if store.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("revoking device: %w", err)
		}
		return audit(ctx, q, userID, "device.revoke", d.ID, map[string]any{
			"name": d.Name, "key_fingerprint": Fingerprint(d.PublicKey),
		})
	})
}

func audit(ctx context.Context, q *db.Queries, actor uuid.UUID, action string, deviceID uuid.UUID, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorUserID: pgtype.UUID{Bytes: actor, Valid: true},
		Action:      action,
		SubjectType: "device",
		SubjectID:   deviceID.String(),
		Data:        b,
	}); err != nil {
		return fmt.Errorf("writing audit log %s: %w", action, err)
	}
	return nil
}
