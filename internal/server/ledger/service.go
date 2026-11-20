package ledger

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/credited"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/evidence"
)

// Limits on event_counts.
const (
	MaxEventCountKeys = 32
	MaxEventCount     = 1_000_000_000
)

// eventKeyRe is the shape of an event_counts key, such as "activity" or
// "ai.prompt_submit".
var eventKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// Errors callers map to HTTP statuses.
var (
	// ErrInvalid wraps malformed fields and bad signatures (400).
	ErrInvalid = errors.New("invalid evidence day")
	// ErrConflict means a different record is stored for the day (409).
	ErrConflict = errors.New("a different record is already stored for this day")
	// ErrChainMismatch means prev_chain_hash doesn't link to the stored chain (409).
	ErrChainMismatch = errors.New("chain mismatch")
	// ErrDeviceNotFound means the device doesn't exist or isn't the caller's (404).
	ErrDeviceNotFound = errors.New("device not found")
	// ErrDeviceRevoked means the device was revoked (403).
	ErrDeviceRevoked = errors.New("device has been revoked")
)

// Store is the database access the service needs. *store.Store satisfies it.
type Store interface {
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
}

// Service checks and stores evidence days.
type Service struct {
	st  Store
	now func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the clock used for the "not in the future" check (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService returns a Service backed by st.
func NewService(st Store, opts ...Option) *Service {
	s := &Service{st: st, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Upload is the request body.
type Upload struct {
	Day           string         `json:"day"`
	PrevChainHash string         `json:"prev_chain_hash"`
	ContentHash   string         `json:"content_hash"`
	ChainHash     string         `json:"chain_hash"`
	Signature     string         `json:"signature"`
	RulesVersion  string         `json:"rules_version"`
	EventCounts   map[string]int `json:"event_counts"`
}

// record is a validated upload.
type record struct {
	day       evidence.Day
	date      time.Time
	chainHash string
	signature []byte
	counts    map[string]int
}

// parse checks field formats; the signature is checked later, once the
// device's key is known.
func parse(deviceID uuid.UUID, u Upload, now time.Time) (record, error) {
	d := evidence.Day{
		DeviceID: deviceID.String(), Day: u.Day, PrevChainHash: u.PrevChainHash,
		ContentHash: u.ContentHash, RulesVersion: u.RulesVersion,
	}
	if err := d.Validate(); err != nil {
		return record{}, fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	}
	// Step 5 re-runs the engine at the recorded version, so a day signed
	// under a version this server doesn't know could never be checked.
	if !credited.ValidRulesVersion(u.RulesVersion) {
		return record{}, fmt.Errorf("%w: unknown rules_version", ErrInvalid)
	}
	date, _ := time.Parse(time.DateOnly, u.Day) // Validate parsed it already
	if today := now.UTC().Truncate(24 * time.Hour); date.After(today) {
		return record{}, fmt.Errorf("%w: day is in the future", ErrInvalid)
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(u.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return record{}, fmt.Errorf("%w: signature must be standard base64 of 64 bytes", ErrInvalid)
	}
	if len(u.EventCounts) > MaxEventCountKeys {
		return record{}, fmt.Errorf("%w: event_counts has more than %d keys", ErrInvalid, MaxEventCountKeys)
	}
	counts := make(map[string]int, len(u.EventCounts))
	for k, v := range u.EventCounts {
		if !eventKeyRe.MatchString(k) || v < 0 || v > MaxEventCount {
			return record{}, fmt.Errorf("%w: event_counts must map short lowercase names to counts", ErrInvalid)
		}
		counts[k] = v
	}
	return record{day: d, date: date, chainHash: u.ChainHash, signature: sig, counts: counts}, nil
}

// Put checks u against the device's key and stored chain and stores it.
// created is false for an identical re-upload. See the package doc for the
// rules.
func (s *Service) Put(ctx context.Context, userID, deviceID uuid.UUID, u Upload) (created bool, err error) {
	rec, err := parse(deviceID, u, s.now())
	if err != nil {
		return false, err
	}
	var conflict bool
	err = s.st.InTx(ctx, func(q *db.Queries) error {
		dev, err := q.DeviceLockForUpload(ctx, db.DeviceLockForUploadParams{ID: deviceID, UserID: userID})
		if store.IsNotFound(err) {
			return ErrDeviceNotFound
		}
		if err != nil {
			return fmt.Errorf("locking device: %w", err)
		}
		if dev.RevokedAt != nil {
			return ErrDeviceRevoked
		}
		// A new key can't sign history from before it existed. The agent
		// starts its chain on the registration day too.
		if y, m, d := dev.CreatedAt.UTC().Date(); rec.date.Before(time.Date(y, m, d, 0, 0, 0, 0, time.UTC)) {
			return fmt.Errorf("%w: day is before the UTC day the device was registered", ErrInvalid)
		}
		if err := evidence.Verify(dev.PublicKey, rec.day, rec.chainHash, rec.signature); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalid, err.Error())
		}
		date := pgtype.Date{Time: rec.date, Valid: true}
		existing, err := q.EvidenceDayGet(ctx, db.EvidenceDayGetParams{DeviceID: deviceID, Day: date})
		switch {
		case err == nil:
			same, err := sameRecord(existing, rec)
			if err != nil || same {
				return err
			}
			conflict = true
			return auditConflict(ctx, q, userID, deviceID, existing, rec)
		case !store.IsNotFound(err):
			return fmt.Errorf("reading evidence day: %w", err)
		}
		n, err := q.EvidenceDayNeighbours(ctx, db.EvidenceDayNeighboursParams{DeviceID: deviceID, Day: date})
		if err != nil {
			return fmt.Errorf("reading evidence chain: %w", err)
		}
		if n.BeforeChainHash != rec.day.PrevChainHash || (n.HasAfter && n.AfterPrevChainHash != rec.chainHash) {
			return ErrChainMismatch
		}
		counts, err := json.Marshal(rec.counts)
		if err != nil {
			return fmt.Errorf("encoding event counts: %w", err)
		}
		if err := q.EvidenceDayInsert(ctx, db.EvidenceDayInsertParams{
			DeviceID: deviceID, Day: date, PrevChainHash: rec.day.PrevChainHash,
			ContentHash: rec.day.ContentHash, ChainHash: rec.chainHash, Signature: rec.signature,
			RulesVersion: rec.day.RulesVersion, EventCounts: counts,
		}); err != nil {
			return fmt.Errorf("storing evidence day: %w", err)
		}
		created = true
		if err := q.DeviceTouch(ctx, db.DeviceTouchParams{SeenAt: s.now().UTC(), ID: deviceID}); err != nil {
			return fmt.Errorf("touching device: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	// The conflict's audit entry is committed above; the upload still fails.
	if conflict {
		return false, ErrConflict
	}
	return created, nil
}

func sameRecord(e db.EvidenceDay, rec record) (bool, error) {
	var stored map[string]int
	if err := json.Unmarshal(e.EventCounts, &stored); err != nil {
		return false, fmt.Errorf("decoding stored event counts: %w", err)
	}
	return e.PrevChainHash == rec.day.PrevChainHash && e.ContentHash == rec.day.ContentHash &&
		e.ChainHash == rec.chainHash && string(e.Signature) == string(rec.signature) &&
		e.RulesVersion == rec.day.RulesVersion && maps.Equal(stored, rec.counts), nil
}

// auditConflict records both versions' hashes (never raw data) so reviewers
// can see what the device signed twice.
func auditConflict(ctx context.Context, q *db.Queries, userID, deviceID uuid.UUID, e db.EvidenceDay, rec record) error {
	data, err := json.Marshal(map[string]any{
		"device_id": deviceID.String(),
		"day":       rec.day.Day,
		"stored":    map[string]string{"chain_hash": e.ChainHash, "content_hash": e.ContentHash, "prev_chain_hash": e.PrevChainHash, "rules_version": e.RulesVersion},
		"uploaded":  map[string]string{"chain_hash": rec.chainHash, "content_hash": rec.day.ContentHash, "prev_chain_hash": rec.day.PrevChainHash, "rules_version": rec.day.RulesVersion},
	})
	if err != nil {
		return fmt.Errorf("encoding audit data: %w", err)
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorUserID: pgtype.UUID{Bytes: userID, Valid: true},
		Action:      "evidence_conflict",
		SubjectType: "device",
		SubjectID:   deviceID.String(),
		Data:        data,
	}); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return nil
}
