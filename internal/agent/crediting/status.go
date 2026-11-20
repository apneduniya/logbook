package crediting

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/apneduniya/logbook/internal/agent/devicekey"
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

	keyEvidenceDevice = "evidence.device"
	keyEvidenceDay    = "evidence.last_day"
	keyEvidenceHash   = "evidence.chain_hash"
	keyEvidenceState  = "evidence.state"
	keyEvidenceError  = "evidence.error"
	// keyEvidenceCursor is "<device id> <day>": every day up to and
	// including <day> has been sealed or found empty for that device.
	keyEvidenceCursor = "evidence.cursor"
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
	// NextAttempt is when a backing-off uploader tries again (live only).
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// Waiting counts minutes not yet accepted by the server.
	Waiting int64 `json:"waiting"`
	// Rejected counts minutes the server refused (never retried).
	Rejected int64 `json:"rejected"`
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
	}
	for _, c := range []struct {
		key string
		dst *int64
	}{{keyRejected, &s.Minutes.Rejected}, {keyWaiting, &s.Minutes.Waiting}} {
		if v := m[c.key]; v != "" {
			if *c.dst, err = strconv.ParseInt(v, 10, 64); err != nil {
				return Status{}, fmt.Errorf("reading %s: %w", c.key, err)
			}
		}
	}
	ev, err := r.Prefix(ctx, "evidence.")
	if err != nil {
		return Status{}, err
	}
	// Chain progress belongs to one device; after re-registration it starts over.
	if ev[keyEvidenceDevice] == s.Device.ID {
		s.Evidence = EvidenceStatus{LastDay: ev[keyEvidenceDay], ChainHash: ev[keyEvidenceHash]}
	}
	s.Evidence.State, s.Evidence.Error = ev[keyEvidenceState], ev[keyEvidenceError]
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
