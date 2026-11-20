package hackatime

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/secretbox"
)

func TestLinkState(t *testing.T) {
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	userA, userB := uuid.New(), uuid.New()
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	st, cookie, err := newLinkState(box, userA, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.State) < 40 || len(st.Verifier) < 43 {
		t.Fatalf("weak state or verifier: %+v", st)
	}

	tests := []struct {
		name   string
		cookie string
		user   uuid.UUID
		state  string
		at     time.Time
		ok     bool
	}{
		{"valid", cookie, userA, st.State, now.Add(time.Minute), true},
		{"other user", cookie, userB, st.State, now, false},
		{"wrong state", cookie, userA, st.State + "x", now, false},
		{"empty state", cookie, userA, "", now, false},
		{"expired", cookie, userA, st.State, now.Add(stateTTL + time.Second), false},
		{"no cookie", "", userA, st.State, now, false},
		{"garbage cookie", "!!!", userA, st.State, now, false},
		{"tampered cookie", cookie[:len(cookie)-2] + "AA", userA, st.State, now, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := openLinkState(box, tt.cookie, tt.user, tt.state, tt.at)
			if tt.ok {
				if err != nil || v != st.Verifier {
					t.Fatalf("openLinkState() = %q, %v", v, err)
				}
				return
			}
			if !errors.Is(err, ErrBadState) {
				t.Fatalf("openLinkState() err = %v, want ErrBadState", err)
			}
		})
	}
}
