package upload

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// State is a worker's health, as shown by the status page and CLI.
type State struct {
	Name        string    `json:"name"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	// Paused, when non-empty, says why the worker isn't sending
	// (ReasonReLogin, "not signed in", "mirroring ... is off").
	Paused string `json:"paused,omitempty"`
	// NextAttempt is when a backing-off worker tries again (live state only).
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// Rejected counts heartbeats the server refused and that were dropped.
	Rejected int64 `json:"rejected"`
}

// Persisted state keys, under "upload.<name>.".
const (
	keyLastSuccess = "last_success"
	keyLastError   = "last_error"
	keyLastErrorAt = "last_error_at"
	keyPaused      = "paused"
	keyRejected    = "rejected"
)

func stateKey(name, field string) string { return "upload." + name + "." + field }

func rejectedKey(name string) string { return stateKey(name, keyRejected) }

// StateReader reads persisted key-value state. *localdb.DB satisfies it.
type StateReader interface {
	Prefix(ctx context.Context, prefix string) (map[string]string, error)
}

// LoadState reads the last persisted state of the named worker. It works
// whether or not the daemon is running.
func LoadState(ctx context.Context, r StateReader, name string) (State, error) {
	m, err := r.Prefix(ctx, "upload."+name+".")
	if err != nil {
		return State{}, err
	}
	st := State{Name: name, LastError: m[stateKey(name, keyLastError)], Paused: m[stateKey(name, keyPaused)]}
	st.LastSuccess = parseTime(m[stateKey(name, keyLastSuccess)])
	st.LastErrorAt = parseTime(m[stateKey(name, keyLastErrorAt)])
	if v := m[rejectedKey(name)]; v != "" {
		if st.Rejected, err = strconv.ParseInt(v, 10, 64); err != nil {
			return State{}, fmt.Errorf("reading rejected count: %w", err)
		}
	}
	return st, nil
}

func parseTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}
