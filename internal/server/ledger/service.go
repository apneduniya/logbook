package ledger

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
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

// MaxBuckets bounds a block's leaf count: the column is a 32-bit integer,
// and one bucket per minute fits for 4,000 years.
const MaxBuckets = math.MaxInt32

// eventKeyRe is the shape of an event_counts key, such as "activity" or
// "ai.prompt_submit".
var eventKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// Errors callers map to HTTP statuses.
var (
	// ErrInvalid wraps malformed fields and bad signatures (400).
	ErrInvalid = errors.New("invalid evidence day")
	// ErrOutdatedAgent means the body has no block fields: it comes from an
	// agent older than ADR 0019 (400).
	ErrOutdatedAgent = errors.New("update the Logbook agent")
	// ErrConflict means a different record is stored for the day (409).
	ErrConflict = errors.New("a different record is already stored for this day")
	// ErrChainMismatch means prev_chain_hash doesn't link to the stored chain (409).
	ErrChainMismatch = errors.New("chain mismatch")
	// ErrSeqMismatch means the record's seq range doesn't continue the
	// stored chain's (409).
	ErrSeqMismatch = errors.New("seq_from must equal the seq_to of the device's previous day")
	// ErrDeviceNotFound means the device doesn't exist or isn't the caller's (404).
	ErrDeviceNotFound = errors.New("device not found")
	// ErrDeviceRevoked means the device was revoked (403).
	ErrDeviceRevoked = errors.New("device has been revoked")
)

// Store is the database access the service needs. *store.Store satisfies it.
type Store interface {
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
	EvidenceDaysOfUserDevice(ctx context.Context, arg db.EvidenceDaysOfUserDeviceParams) ([]db.EvidenceDay, error)
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

// Upload is the request body. The integer block fields are pointers so a
// body without them (an agent older than ADR 0019) can be told apart from
// zeros.
type Upload struct {
	Day           string         `json:"day"`
	PrevChainHash string         `json:"prev_chain_hash"`
	ContentHash   string         `json:"content_hash"`
	ChainHash     string         `json:"chain_hash"`
	Signature     string         `json:"signature"`
	RulesVersion  string         `json:"rules_version"`
	EventCounts   map[string]int `json:"event_counts"`
	SeqFrom       *int64         `json:"seq_from"`
	SeqTo         *int64         `json:"seq_to"`
	Buckets       *int64         `json:"buckets"`
	TimeFrom      *time.Time     `json:"time_from"`
	TimeTo        *time.Time     `json:"time_to"`
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
	if u.SeqFrom == nil || u.SeqTo == nil || u.Buckets == nil {
		return record{}, ErrOutdatedAgent
	}
	if *u.Buckets > MaxBuckets {
		return record{}, fmt.Errorf("%w: buckets is more than %d", ErrInvalid, MaxBuckets)
	}
	from, err := utcTime(u.TimeFrom)
	if err != nil {
		return record{}, err
	}
	to, err := utcTime(u.TimeTo)
	if err != nil {
		return record{}, err
	}
	d := evidence.Day{
		DeviceID: deviceID.String(), Day: u.Day, PrevChainHash: u.PrevChainHash,
		ContentHash: u.ContentHash, RulesVersion: u.RulesVersion,
		SeqFrom: *u.SeqFrom, SeqTo: *u.SeqTo, Buckets: int(*u.Buckets), TimeFrom: from, TimeTo: to,
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

// utcTime checks that t (time_from or time_to) is in UTC, like every other
// time the API takes, and returns a copy in time.UTC. nil stays nil.
func utcTime(t *time.Time) (*time.Time, error) {
	if t == nil {
		return nil, nil
	}
	if _, off := t.Zone(); off != 0 {
		return nil, fmt.Errorf("%w: time_from and time_to must be RFC 3339 UTC times", ErrInvalid)
	}
	u := t.UTC()
	return &u, nil
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
		if err := checkLinks(ctx, q, deviceID, date, rec); err != nil {
			return err
		}
		counts, err := json.Marshal(rec.counts)
		if err != nil {
			return fmt.Errorf("encoding event counts: %w", err)
		}
		if err := q.EvidenceDayInsert(ctx, db.EvidenceDayInsertParams{
			DeviceID: deviceID, Day: date, PrevChainHash: rec.day.PrevChainHash,
			ContentHash: rec.day.ContentHash, ChainHash: rec.chainHash, Signature: rec.signature,
			RulesVersion: rec.day.RulesVersion, EventCounts: counts,
			SeqFrom: rec.day.SeqFrom, SeqTo: rec.day.SeqTo,
			Buckets:  int32(rec.day.Buckets), //nolint:gosec // parse caps it at MaxBuckets
			TimeFrom: rec.day.TimeFrom, TimeTo: rec.day.TimeTo,
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

// checkLinks checks that rec continues the device's stored chain: its
// prev_chain_hash is the chain hash of the latest stored day before it (""
// when there is none) and its seq_from is that day's seq_to; and the
// earliest stored day after it, if any, continues rec the same way.
//
// The device's first stored day may start at any seq_from: 0 on a fresh
// install, or the log's high-water mark when the device took over from
// another (ADR 0019 point 4). The server can't tell those apart, and a
// later start only leaves rows unsealed, which a submission can't use. A
// stored day without block fields (from before ADR 0019, dev only) doesn't
// constrain seq_from.
func checkLinks(ctx context.Context, q *db.Queries, deviceID uuid.UUID, date pgtype.Date, rec record) error {
	before, err := q.EvidenceDayBefore(ctx, db.EvidenceDayBeforeParams{DeviceID: deviceID, Day: date})
	switch {
	case store.IsNotFound(err):
		if rec.day.PrevChainHash != "" {
			return ErrChainMismatch
		}
	case err != nil:
		return fmt.Errorf("reading the previous evidence day: %w", err)
	default:
		if before.ChainHash != rec.day.PrevChainHash {
			return ErrChainMismatch
		}
		if before.SeqTo.Valid && before.SeqTo.Int64 != rec.day.SeqFrom {
			return ErrSeqMismatch
		}
	}
	after, err := q.EvidenceDayAfter(ctx, db.EvidenceDayAfterParams{DeviceID: deviceID, Day: date})
	switch {
	case store.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("reading the next evidence day: %w", err)
	}
	if after.PrevChainHash != rec.chainHash {
		return ErrChainMismatch
	}
	if after.SeqFrom.Valid && after.SeqFrom.Int64 != rec.day.SeqTo {
		return ErrSeqMismatch
	}
	return nil
}

func sameRecord(e db.EvidenceDay, rec record) (bool, error) {
	var stored map[string]int
	if err := json.Unmarshal(e.EventCounts, &stored); err != nil {
		return false, fmt.Errorf("decoding stored event counts: %w", err)
	}
	return e.PrevChainHash == rec.day.PrevChainHash && e.ContentHash == rec.day.ContentHash &&
		e.ChainHash == rec.chainHash && string(e.Signature) == string(rec.signature) &&
		e.RulesVersion == rec.day.RulesVersion && maps.Equal(stored, rec.counts) &&
		e.SeqFrom.Valid && e.SeqFrom.Int64 == rec.day.SeqFrom &&
		e.SeqTo.Valid && e.SeqTo.Int64 == rec.day.SeqTo &&
		e.Buckets.Valid && int(e.Buckets.Int32) == rec.day.Buckets &&
		sameTime(e.TimeFrom, rec.day.TimeFrom) && sameTime(e.TimeTo, rec.day.TimeTo), nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// auditConflict records both versions' hashes and seq ranges (never raw
// data) so reviewers can see what the device signed twice.
func auditConflict(ctx context.Context, q *db.Queries, userID, deviceID uuid.UUID, e db.EvidenceDay, rec record) error {
	stored := map[string]any{"chain_hash": e.ChainHash, "content_hash": e.ContentHash, "prev_chain_hash": e.PrevChainHash, "rules_version": e.RulesVersion}
	if e.SeqFrom.Valid && e.SeqTo.Valid {
		stored["seq_from"], stored["seq_to"] = e.SeqFrom.Int64, e.SeqTo.Int64
	}
	data, err := json.Marshal(map[string]any{
		"device_id": deviceID.String(),
		"day":       rec.day.Day,
		"stored":    stored,
		"uploaded": map[string]any{
			"chain_hash": rec.chainHash, "content_hash": rec.day.ContentHash, "prev_chain_hash": rec.day.PrevChainHash,
			"rules_version": rec.day.RulesVersion, "seq_from": rec.day.SeqFrom, "seq_to": rec.day.SeqTo,
		},
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
