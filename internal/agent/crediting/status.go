package crediting

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
)

// Persisted state keys. The status page reads them, so they also work when
// the daemon is stopped.
const (
	keyLastComputed = "crediting.last_computed"
	keyLastSuccess  = "crediting.last_success"
	keyLastError    = "crediting.last_error"
	keyLastErrorAt  = "crediting.last_error_at"
	keyPaused       = "crediting.paused"
	keyRejected     = "crediting.rejected"
	keyWaiting      = "crediting.waiting"
	// keyRefused says why minute uploads are on hold after the server
	// refused a batch as a whole ("" when not).
	keyRefused = "crediting.refused"
	// keySealedRejected counts minutes the server refused because their
	// day was already sealed, and keySealedError describes the latest.
	// Both should stay empty: a day is sealed only after its minutes.
	keySealedRejected = "crediting.sealed_rejected"
	keySealedError    = "crediting.sealed_error"
	// keyClock describes a clock problem ("" when none); see
	// MinutesStatus.Clock.
	keyClock = "crediting.clock"
	// keySealedThrough is the latest UTC day (YYYY-MM-DD) the evidence
	// chain has moved past, sealed or found empty. It only grows; no minute
	// before that day's end is computed or stored again.
	keySealedThrough = "crediting.sealed_through"
	// keySettling says why no minute becomes final yet after a start or a
	// wake ("" once settled).
	keySettling = "crediting.settling"
	// keyLateIgnored counts raw events logged after their minute was final,
	// and keyLateLatest is the latest such event's time. keyLateMark is the
	// log's high-water mark up to which rows have been checked.
	keyLateIgnored = "crediting.late_ignored"
	keyLateLatest  = "crediting.late_latest"
	keyLateMark    = "crediting.late_mark"
	// keyUnkeyed counts rows adopted from the server that carry no
	// computation key (stored before keys existed): they can't be
	// replayed.
	keyUnkeyed = "crediting.unkeyed"

	keyEvidenceDevice = "evidence.device"
	keyEvidenceDay    = "evidence.last_day"
	keyEvidenceHash   = "evidence.chain_hash"
	keyEvidenceState  = "evidence.state"
	keyEvidenceError  = "evidence.error"
	// keyEvidenceLocal is the last local failure sealing a day ("" when
	// the last attempt worked).
	keyEvidenceLocal = "evidence.local_error"
	// keyEvidenceCursor is "<device id> <day>": every day up to and
	// including <day> has been sealed or found empty for that device.
	keyEvidenceCursor = "evidence.cursor"
	// keyEvidenceProgress holds a large block's hashing progress between
	// runs (sealProgress), "" when none.
	keyEvidenceProgress = "evidence.progress"
)

// Status is what the status page and `logbook status` show about crediting.
// It holds no secrets: the device id and chain hashes are public to the
// server and to anyone the user shares evidence with.
type Status struct {
	Device   DeviceStatus   `json:"device"`
	Minutes  MinutesStatus  `json:"minutes"`
	Evidence EvidenceStatus `json:"evidence"`
}

// DeviceStatus is the device's registration.
type DeviceStatus struct {
	ID           string    `json:"id,omitempty"`
	RegisteredAt time.Time `json:"registered_at,omitzero"`
	// RevokedAt is set while the device is revoked.
	RevokedAt time.Time `json:"revoked_at,omitzero"`
}

// MinutesStatus is the credited-minute uploader's health.
type MinutesStatus struct {
	LastComputed time.Time `json:"last_computed,omitzero"`
	LastSuccess  time.Time `json:"last_success,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitzero"`
	// Paused, when non-empty, says why nothing is uploaded.
	Paused string `json:"paused,omitempty"`
	// Refused, when non-empty, says why minute uploads are on hold: the
	// server refused a batch as a whole (not for its size). Minutes are
	// kept and retried later.
	Refused string `json:"refused,omitempty"`
	// NextAttempt is when a backing-off uploader tries again (live only).
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// Waiting counts minutes not yet accepted by the server.
	Waiting int64 `json:"waiting"`
	// Rejected counts minutes the server refused (never retried).
	Rejected int64 `json:"rejected"`
	// SealedRejected counts the rejected minutes whose UTC day the server
	// had already sealed, and SealedError describes the latest. These are
	// lost credit and point to a bug: the agent seals a day only after the
	// server has accepted every minute of it.
	SealedRejected int64  `json:"sealed_rejected,omitempty"`
	SealedError    string `json:"sealed_error,omitempty"`
	// Clock, when non-empty, describes a problem with the system clock:
	// it is behind the last sealed day (crediting is paused until it
	// catches up), or it differs from the server's by more than
	// MaxClockSkew, or the server gave no usable time (no day is sealed
	// until the clocks agree; minutes are still uploaded).
	Clock string `json:"clock,omitempty"`
	// Settling, when non-empty, says why no minute becomes final yet: the
	// agent has just started (or woken) and waits for late events, or for
	// the hook spool to drain (ADR 0017).
	Settling string `json:"settling,omitempty"`
	// LateIgnored counts raw events logged after the minute they belong to
	// was final; they are kept and sealed as evidence but never change its
	// credit. LateLatest is the latest such event's time.
	LateIgnored int64     `json:"late_ignored,omitempty"`
	LateLatest  time.Time `json:"late_latest,omitzero"`
	// Unkeyed counts rows adopted from the server without a computation
	// key (stored before the server kept keys); they can't be replayed.
	Unkeyed int64 `json:"unkeyed,omitempty"`
	// DroppedSamples counts activity samples not stored because the local
	// log was full of rows the evidence chain hasn't sealed yet.
	DroppedSamples int64 `json:"dropped_samples,omitempty"`
}

// EvidenceStatus is the evidence chain's progress.
type EvidenceStatus struct {
	// LastDay is the latest UTC day the server accepted.
	LastDay string `json:"last_day,omitempty"`
	// ChainHash is that day's chain hash.
	ChainHash string `json:"chain_hash,omitempty"`
	// State is "" (fine), "conflict" or "rejected"; the chain stops on
	// either until fixed.
	State string `json:"state,omitempty"`
	Error string `json:"error,omitempty"`
	// LocalError is the last local failure sealing a day (reading the
	// database, hashing); sealing is retried every run.
	LocalError string `json:"local_error,omitempty"`
}

// StateReader reads persisted key-value state. *localdb.DB satisfies it.
type StateReader interface {
	Prefix(ctx context.Context, prefix string) (map[string]string, error)
}

// LoadStatus reads the persisted crediting state.
func LoadStatus(ctx context.Context, r StateReader) (Status, error) {
	var s Status
	dev, err := r.Prefix(ctx, "device.")
	if err != nil {
		return Status{}, err
	}
	s.Device = DeviceStatus{
		ID: dev[devicekey.KeyID], RegisteredAt: parseTime(dev[devicekey.KeyRegisteredAt]),
		RevokedAt: parseTime(dev[devicekey.KeyRevokedAt]),
	}
	m, err := r.Prefix(ctx, "crediting.")
	if err != nil {
		return Status{}, err
	}
	s.Minutes = MinutesStatus{
		LastComputed: parseTime(m[keyLastComputed]), LastSuccess: parseTime(m[keyLastSuccess]),
		LastError: m[keyLastError], LastErrorAt: parseTime(m[keyLastErrorAt]), Paused: m[keyPaused],
		Refused: m[keyRefused], SealedError: m[keySealedError], Clock: m[keyClock],
		Settling: m[keySettling], LateLatest: parseTime(m[keyLateLatest]),
	}
	for _, c := range []struct {
		key string
		dst *int64
	}{
		{keyRejected, &s.Minutes.Rejected}, {keyWaiting, &s.Minutes.Waiting},
		{keySealedRejected, &s.Minutes.SealedRejected}, {keyLateIgnored, &s.Minutes.LateIgnored},
		{keyUnkeyed, &s.Minutes.Unkeyed},
	} {
		if v := m[c.key]; v != "" {
			if *c.dst, err = strconv.ParseInt(v, 10, 64); err != nil {
				return Status{}, fmt.Errorf("reading %s: %w", c.key, err)
			}
		}
	}
	logState, err := r.Prefix(ctx, localdb.KeyDroppedSamples)
	if err != nil {
		return Status{}, err
	}
	if v := logState[localdb.KeyDroppedSamples]; v != "" {
		if s.Minutes.DroppedSamples, err = strconv.ParseInt(v, 10, 64); err != nil {
			return Status{}, fmt.Errorf("reading %s: %w", localdb.KeyDroppedSamples, err)
		}
	}
	ev, err := r.Prefix(ctx, "evidence.")
	if err != nil {
		return Status{}, err
	}
	// The chain belongs to one device; after re-registration it starts over.
	if ev[keyEvidenceDevice] == s.Device.ID {
		s.Evidence = EvidenceStatus{LastDay: ev[keyEvidenceDay], ChainHash: ev[keyEvidenceHash],
			State: ev[keyEvidenceState], Error: ev[keyEvidenceError]}
	}
	s.Evidence.LocalError = ev[keyEvidenceLocal]
	return s, nil
}

func parseTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
