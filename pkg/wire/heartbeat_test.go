package wire

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestValidate(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ok := Heartbeat{Entity: "/src/main.go", Time: float64(now.Unix()) - 30}

	tests := []struct {
		name    string
		mutate  func(*Heartbeat)
		wantErr bool
	}{
		{"valid", func(*Heartbeat) {}, false},
		{"missing entity", func(h *Heartbeat) { h.Entity = " " }, true},
		{"future", func(h *Heartbeat) { h.Time = float64(now.Add(2 * time.Hour).Unix()) }, true},
		{"ancient", func(h *Heartbeat) { h.Time = 1 }, true},
		{"long project", func(h *Heartbeat) { h.Project = string(make([]byte, 300)) }, true},
		{"too many deps", func(h *Heartbeat) { h.Dependencies = make([]string, 101) }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := ok
			tt.mutate(&h)
			err := h.Validate(now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidHeartbeat) {
				t.Fatalf("error %v does not wrap ErrInvalidHeartbeat", err)
			}
		})
	}
}

func TestFieldsHash(t *testing.T) {
	a := Heartbeat{Entity: "a.go", Time: 1795000000.5, Project: "x", Lineno: ptr(3)}
	b := a
	if !bytes.Equal(a.FieldsHash(), b.FieldsHash()) {
		t.Fatal("identical heartbeats hash differently")
	}
	b.Lineno = ptr(4)
	if bytes.Equal(a.FieldsHash(), b.FieldsHash()) {
		t.Fatal("different lineno hashes the same")
	}
	// Field boundaries must not collide.
	c := Heartbeat{Entity: "ab", Project: "c", Time: a.Time}
	d := Heartbeat{Entity: "a", Project: "bc", Time: a.Time}
	if bytes.Equal(c.FieldsHash(), d.FieldsHash()) {
		t.Fatal("boundary collision")
	}
}

func TestTimestamp(t *testing.T) {
	h := Heartbeat{Time: 1795000000.25}
	if got := h.Timestamp(); got.Unix() != 1795000000 || got.Nanosecond() != 250_000_000 {
		t.Fatalf("Timestamp() = %v", got)
	}
}
