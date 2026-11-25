package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Key format: KeyPrefix plus 256 random bits in unpadded base64url (43
// characters). The stored prefix is KeyPrefix plus the first 12 of them.
const (
	KeyPrefix     = "lba_"
	keyRandom     = 32
	keyLen        = len(KeyPrefix) + 43
	keyPrefixLen  = len(KeyPrefix) + 12
	maxKeyNameLen = 64
	// keyTouchEvery is how often a key's last_used_at is written.
	keyTouchEvery = time.Minute
)

// Roles a user can have (users.role).
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

var (
	// ErrInvalidKey means a credential isn't a live admin key of an admin.
	ErrInvalidKey = errors.New("invalid admin key")
	// ErrUserNotFound means no user has the username.
	ErrUserNotFound = errors.New("user not found")
	// ErrNotAdmin means the user's role isn't admin.
	ErrNotAdmin = errors.New("the user isn't an admin; run `logbook-server admin grant <username>` first")
	// ErrKeyNotFound means no live key has the prefix.
	ErrKeyNotFound = errors.New("no live admin key has that prefix")
	// ErrInvalidName means a key name is empty, too long or has control
	// characters.
	ErrInvalidName = fmt.Errorf("a key name must be 1-%d characters with no control characters", maxKeyNameLen)
)

// KeyStore is the database access Keys needs; *store.Store implements it.
type KeyStore interface {
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
	AdminKeysList(ctx context.Context) ([]db.AdminKeysListRow, error)
	AdminKeyByPrefix(ctx context.Context, prefix string) (db.AdminKeyByPrefixRow, error)
	AdminKeyTouch(ctx context.Context, arg db.AdminKeyTouchParams) error
	AdminAuditReads(ctx context.Context, arg db.AdminAuditReadsParams) error
	AdminAuditCommand(ctx context.Context, arg db.AdminAuditCommandParams) error
}

// Keys manages the admin role and admin keys, and authenticates keys.
type Keys struct {
	st  KeyStore
	now func() time.Time
}

// NewKeys returns Keys backed by st. now may be nil (time.Now).
func NewKeys(st KeyStore, now func() time.Time) *Keys {
	if now == nil {
		now = time.Now
	}
	return &Keys{st: st, now: func() time.Time { return now().UTC() }}
}

// hashKey is the stored form of a key.
func hashKey(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

// newKey returns a fresh key.
func newKey() string {
	b := make([]byte, keyRandom)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// wellFormed reports whether s has an admin key's shape.
func wellFormed(s string) bool {
	if len(s) != keyLen || !strings.HasPrefix(s, KeyPrefix) {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(s[len(KeyPrefix):])
	return err == nil
}

// Admin is who an admin key belongs to.
type Admin struct {
	UserID   uuid.UUID
	PublicID int64
	Username string
	KeyID    uuid.UUID
	// KeyPrefix names the key in audit rows; never the key itself.
	KeyPrefix string
}

// Authenticate resolves an admin key: ErrInvalidKey unless it is well
// formed, stored (by its SHA-256, compared in constant time), not revoked,
// and its user's role is admin.
func (k *Keys) Authenticate(ctx context.Context, key string) (Admin, error) {
	if !wellFormed(key) {
		return Admin{}, ErrInvalidKey
	}
	row, err := k.st.AdminKeyByPrefix(ctx, key[:keyPrefixLen])
	if store.IsNotFound(err) {
		return Admin{}, ErrInvalidKey
	}
	if err != nil {
		return Admin{}, fmt.Errorf("looking up admin key: %w", err)
	}
	if subtle.ConstantTimeCompare(hashKey(key), row.KeyHash) != 1 || row.RevokedAt != nil || row.Role != RoleAdmin {
		return Admin{}, ErrInvalidKey
	}
	now := k.now()
	if row.LastUsedAt == nil || now.Sub(*row.LastUsedAt) >= keyTouchEvery {
		if err := k.st.AdminKeyTouch(ctx, db.AdminKeyTouchParams{
			UsedAt: now, ID: row.ID, TouchBefore: now.Add(-keyTouchEvery),
		}); err != nil {
			return Admin{}, fmt.Errorf("touching admin key: %w", err)
		}
	}
	return Admin{UserID: row.UserID, PublicID: row.PublicID, Username: row.Username, KeyID: row.ID, KeyPrefix: row.Prefix}, nil
}

// AuditRead writes the admin.read rows of one request: one per subject
// user, or one about the key when the request read no user.
func (k *Keys) AuditRead(ctx context.Context, a Admin, subjects []uuid.UUID, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	arg := db.AdminAuditReadsParams{ActorUserID: a.UserID, SubjectType: "user", Data: b}
	for _, s := range subjects {
		arg.SubjectIds = append(arg.SubjectIds, s.String())
	}
	if len(arg.SubjectIds) == 0 {
		arg.SubjectType, arg.SubjectIds = "admin_key", []string{a.KeyID.String()}
	}
	if err := k.st.AdminAuditReads(ctx, arg); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return nil
}

// auditCommand writes the audit row of a command-line action.
func auditCommand(ctx context.Context, q *db.Queries, action, subjectType, subjectID string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	data["via"] = "command line"
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	if err := q.AdminAuditCommand(ctx, db.AdminAuditCommandParams{
		Action: action, SubjectType: subjectType, SubjectID: subjectID, Data: b,
	}); err != nil {
		return fmt.Errorf("writing audit log %s: %w", action, err)
	}
	return nil
}

// lockUser locks a user by username.
func lockUser(ctx context.Context, q *db.Queries, username string) (db.AdminUserForUpdateRow, error) {
	u, err := q.AdminUserForUpdate(ctx, username)
	if store.IsNotFound(err) {
		return u, ErrUserNotFound
	}
	if err != nil {
		return u, fmt.Errorf("reading user: %w", err)
	}
	return u, nil
}

// Grant makes a user an admin. changed is false when they already were.
// Audited as admin.grant.
func (k *Keys) Grant(ctx context.Context, username string) (changed bool, err error) {
	err = k.st.InTx(ctx, func(q *db.Queries) error {
		u, err := lockUser(ctx, q, username)
		if err != nil {
			return err
		}
		changed = u.Role != RoleAdmin
		if changed {
			if err := q.AdminSetRole(ctx, db.AdminSetRoleParams{ID: u.ID, Role: RoleAdmin}); err != nil {
				return fmt.Errorf("setting role: %w", err)
			}
		}
		return auditCommand(ctx, q, "admin.grant", "user", u.ID.String(),
			map[string]any{"username": u.Username, "changed": changed})
	})
	return changed, err
}

// Revoke takes the admin role from a user and revokes every admin key of
// theirs, returning how many keys it revoked. Audited as admin.revoke.
func (k *Keys) Revoke(ctx context.Context, username string) (revoked int64, err error) {
	err = k.st.InTx(ctx, func(q *db.Queries) error {
		u, err := lockUser(ctx, q, username)
		if err != nil {
			return err
		}
		if u.Role == RoleAdmin {
			if err := q.AdminSetRole(ctx, db.AdminSetRoleParams{ID: u.ID, Role: RoleUser}); err != nil {
				return fmt.Errorf("setting role: %w", err)
			}
		}
		if revoked, err = q.AdminKeysRevokeOfUser(ctx, db.AdminKeysRevokeOfUserParams{UserID: u.ID, RevokedAt: k.now()}); err != nil {
			return fmt.Errorf("revoking admin keys: %w", err)
		}
		return auditCommand(ctx, q, "admin.revoke", "user", u.ID.String(),
			map[string]any{"username": u.Username, "was_admin": u.Role == RoleAdmin, "keys_revoked": revoked})
	})
	return revoked, err
}

// NewKey is a key as created: Key is shown once and never stored.
type NewKey struct {
	Key    string
	Prefix string
}

// CreateKey makes an admin key for an admin. Audited as admin_key.create
// (name and prefix, never the key).
func (k *Keys) CreateKey(ctx context.Context, username, name string) (NewKey, error) {
	name = strings.TrimSpace(name)
	if !validName(name) {
		return NewKey{}, ErrInvalidName
	}
	var out NewKey
	err := k.st.InTx(ctx, func(q *db.Queries) error {
		u, err := lockUser(ctx, q, username)
		if err != nil {
			return err
		}
		if u.Role != RoleAdmin {
			return ErrNotAdmin
		}
		// A prefix holds 72 random bits; retry on the off chance one is taken.
		for range 3 {
			key := newKey()
			id, err := q.AdminKeyInsert(ctx, db.AdminKeyInsertParams{
				UserID: u.ID, Name: name, Prefix: key[:keyPrefixLen], KeyHash: hashKey(key), CreatedAt: k.now(),
			})
			if store.IsNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("storing admin key: %w", err)
			}
			out = NewKey{Key: key, Prefix: key[:keyPrefixLen]}
			return auditCommand(ctx, q, "admin_key.create", "admin_key", id.String(),
				map[string]any{"username": u.Username, "name": name, "prefix": out.Prefix})
		}
		return errors.New("could not pick an unused key prefix")
	})
	return out, err
}

// KeyInfo is an admin key as listed.
type KeyInfo struct {
	Prefix     string
	Username   string
	Name       string
	UserRole   string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// ListKeys lists every admin key, revoked ones included, oldest first.
// Audited as admin_key.list.
func (k *Keys) ListKeys(ctx context.Context) ([]KeyInfo, error) {
	rows, err := k.st.AdminKeysList(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing admin keys: %w", err)
	}
	if err := k.st.InTx(ctx, func(q *db.Queries) error {
		return auditCommand(ctx, q, "admin_key.list", "admin_key", "*", map[string]any{"keys": len(rows)})
	}); err != nil {
		return nil, err
	}
	out := make([]KeyInfo, len(rows))
	for i, r := range rows {
		out[i] = KeyInfo{
			Prefix: r.Prefix, Username: r.Username, Name: r.Name, UserRole: r.Role,
			CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, RevokedAt: r.RevokedAt,
		}
	}
	return out, nil
}

// RevokeKey revokes one live key by its prefix. Audited as
// admin_key.revoke.
func (k *Keys) RevokeKey(ctx context.Context, prefix string) error {
	return k.st.InTx(ctx, func(q *db.Queries) error {
		r, err := q.AdminKeyRevoke(ctx, db.AdminKeyRevokeParams{Prefix: strings.TrimSpace(prefix), RevokedAt: k.now()})
		if store.IsNotFound(err) {
			return ErrKeyNotFound
		}
		if err != nil {
			return fmt.Errorf("revoking admin key: %w", err)
		}
		return auditCommand(ctx, q, "admin_key.revoke", "admin_key", r.ID.String(),
			map[string]any{"username": r.Username, "name": r.Name, "prefix": r.Prefix})
	})
}

func validName(name string) bool {
	if name == "" || len([]rune(name)) > maxKeyNameLen {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return false
		}
	}
	return true
}
