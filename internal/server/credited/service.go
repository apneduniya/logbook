package credited

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Errors for the whole request. Handlers map them to HTTP statuses.
var (
	// ErrInvalidBatch wraps request-level validation failures (400).
	ErrInvalidBatch = errors.New("invalid minutes batch")
	// ErrDeviceNotFound means the device doesn't exist or isn't the caller's (404).
	ErrDeviceNotFound = errors.New("device not found")
	// ErrDeviceRevoked means the device was revoked; uploads stop (403).
	ErrDeviceRevoked = errors.New("device has been revoked")
)

// Publisher is told which of a user's minutes changed, after commit.
// live.Hub implements it, so open dashboards refetch.
type Publisher interface {
	MinutesChanged(userID uuid.UUID, from, to time.Time)
}

// Store is the database access the service needs. *store.Store satisfies it.
type Store interface {
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
}

// Service stores uploaded minutes.
type Service struct {
	st  Store
	pub Publisher
	now func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the server clock used for the rewrite window and the
// future check (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService returns a Service. pub may be nil.
func NewService(st Store, pub Publisher, opts ...Option) *Service {
	s := &Service{st: st, pub: pub, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Batch is one upload request.
type Batch struct {
	RulesVersion string    `json:"rules_version"`
	Minutes      []RawItem `json:"minutes"`
}

// Result is the outcome of an upload. Rejected is sorted by index.
type Result struct {
	Accepted int         `json:"accepted"`
	Rejected []Rejection `json:"rejected"`
}

// Upload validates b and stores the accepted minutes for one of userID's
// devices, in one transaction.
func (s *Service) Upload(ctx context.Context, userID, deviceID uuid.UUID, b Batch) (Result, error) {
	if len(b.Minutes) > MaxBatch {
		return Result{}, fmt.Errorf("%w: at most %d minutes per request", ErrInvalidBatch, MaxBatch)
	}
	if !ValidRulesVersion(b.RulesVersion) {
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidBatch, errUnknownVersion)
	}
	now := s.now().UTC()
	valid, rejected := Parse(b.Minutes, now)

	var write, remove []Item
	err := s.st.InTx(ctx, func(q *db.Queries) error {
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
		if len(valid) == 0 {
			return nil
		}
		minutes := minutesOf(valid)
		stored, err := q.CreditedMinutesForDevice(ctx, db.CreditedMinutesForDeviceParams{
			DeviceID: deviceID, Minutes: minutes,
		})
		if err != nil {
			return fmt.Errorf("reading stored minutes: %w", err)
		}
		sealed, err := q.EvidenceDaysSealed(ctx, db.EvidenceDaysSealedParams{
			DeviceID: deviceID,
			FirstDay: pgtype.Date{Time: utcDay(minutes[0]), Valid: true},
			LastDay:  pgtype.Date{Time: utcDay(minutes[len(minutes)-1]), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("reading sealed days: %w", err)
		}
		bounds := Bounds{RulesVersion: b.RulesVersion, Now: now, Since: utcDay(dev.CreatedAt)}
		for _, d := range sealed {
			bounds.Sealed = append(bounds.Sealed, d.Time)
		}
		var more []Rejection
		write, remove, more = Plan(valid, toStored(stored), bounds)
		rejected = append(rejected, more...)
		// Removals first: they free the slots their replacements take.
		if err := deleteRows(ctx, q, deviceID, remove); err != nil {
			return err
		}
		if err := upsert(ctx, q, userID, deviceID, b.RulesVersion, now, write); err != nil {
			return err
		}
		if err := q.DeviceTouch(ctx, db.DeviceTouchParams{SeenAt: now, ID: deviceID}); err != nil {
			return fmt.Errorf("touching device: %w", err)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if changed := slices.Concat(write, remove); len(changed) > 0 && s.pub != nil {
		from, to := changed[0].Minute, changed[0].Minute
		for _, it := range changed {
			from, to = minTime(from, it.Minute), maxTime(to, it.Minute)
		}
		s.pub.MinutesChanged(userID, from, to.Add(time.Minute))
	}
	slices.SortFunc(rejected, func(a, b Rejection) int { return a.Index - b.Index })
	if rejected == nil {
		rejected = []Rejection{}
	}
	return Result{Accepted: len(b.Minutes) - len(rejected), Rejected: rejected}, nil
}

// minutesOf returns the distinct minutes of items, sorted.
func minutesOf(items []Indexed) []time.Time {
	out := make([]time.Time, 0, len(items))
	for _, it := range items {
		out = append(out, it.Item.Minute)
	}
	slices.SortFunc(out, time.Time.Compare)
	return slices.CompactFunc(out, time.Time.Equal)
}

func toStored(rows []db.CreditedMinutesForDeviceRow) []Stored {
	out := make([]Stored, len(rows))
	for i, r := range rows {
		cuts, flags := slices.Sorted(slices.Values(r.Cuts)), slices.Sorted(slices.Values(r.Flags))
		out[i] = Stored{
			Item: Item{
				Minute: r.Minute.UTC(), Project: r.Project, Label: r.Label, Credited: int(r.Credited),
				Cuts: slices.Compact(cuts), Flags: slices.Compact(flags),
			},
			RulesVersion: r.RulesVersion,
			UpdatedAt:    r.UpdatedAt.UTC(),
		}
	}
	return out
}

// upsertRow is one element of the JSON array CreditedMinutesUpsert reads.
// Its fields match Item's, so an Item converts to it directly.
type upsertRow struct {
	Minute   time.Time `json:"minute"`
	Project  string    `json:"project"`
	Label    string    `json:"label"`
	Credited int       `json:"credited"`
	Cuts     []string  `json:"cuts"`
	Flags    []string  `json:"flags"`
}

// deleteRow is one element of the JSON array CreditedMinutesDelete reads.
type deleteRow struct {
	Minute  time.Time `json:"minute"`
	Project string    `json:"project"`
}

func deleteRows(ctx context.Context, q *db.Queries, deviceID uuid.UUID, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	rows := make([]deleteRow, len(items))
	for i, it := range items {
		rows[i] = deleteRow{Minute: it.Minute, Project: it.Project}
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("encoding removed minutes: %w", err)
	}
	if err := q.CreditedMinutesDelete(ctx, db.CreditedMinutesDeleteParams{DeviceID: deviceID, Rows: payload}); err != nil {
		return fmt.Errorf("removing minutes: %w", err)
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func upsert(ctx context.Context, q *db.Queries, userID, deviceID uuid.UUID, rulesVersion string, now time.Time, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	rows := make([]upsertRow, len(items))
	for i, it := range items {
		rows[i] = upsertRow(it)
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("encoding minutes: %w", err)
	}
	if err := q.CreditedMinutesUpsert(ctx, db.CreditedMinutesUpsertParams{
		DeviceID: deviceID, UserID: userID, RulesVersion: rulesVersion, UpdatedAt: now, Rows: payload,
	}); err != nil {
		return fmt.Errorf("writing minutes: %w", err)
	}
	return nil
}
