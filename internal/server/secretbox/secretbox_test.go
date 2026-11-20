package secretbox_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/apneduniya/logbook/internal/server/secretbox"
)

func newBox(t *testing.T, fill byte) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(bytes.Repeat([]byte{fill}, secretbox.KeySize))
	if err != nil {
		t.Fatalf("New() err = %v", err)
	}
	return b
}

func TestNewKeyLength(t *testing.T) {
	tests := []struct {
		n       int
		wantErr bool
	}{
		{0, true}, {16, true}, {24, true}, {31, true}, {33, true}, {64, true}, {32, false},
	}
	for _, tt := range tests {
		_, err := secretbox.New(make([]byte, tt.n))
		if (err != nil) != tt.wantErr {
			t.Errorf("New(%d-byte key) err = %v, wantErr %v", tt.n, err, tt.wantErr)
		}
	}
}

func TestRoundTripAndRandomNonce(t *testing.T) {
	b := newBox(t, 1)
	aad := []byte("user-1")
	c1, err := b.Seal([]byte("secret-token"), aad)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := b.Seal([]byte("secret-token"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(c1, c2) {
		t.Fatal("two seals of the same plaintext are identical; nonce not random")
	}
	if bytes.Contains(c1, []byte("secret-token")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := b.Open(c1, aad)
	if err != nil || string(got) != "secret-token" {
		t.Fatalf("Open() = %q, %v", got, err)
	}
}

func TestOpenRejects(t *testing.T) {
	b := newBox(t, 1)
	aad := []byte("user-1")
	ct, err := b.Seal([]byte("secret-token"), aad)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		c := bytes.Clone(ct)
		c[i] ^= 0x01
		return c
	}
	tests := []struct {
		name string
		box  *secretbox.Box
		ct   []byte
		aad  []byte
	}{
		{"wrong aad", b, ct, []byte("user-2")},
		{"empty aad", b, ct, nil},
		{"wrong key", newBox(t, 2), ct, aad},
		{"tampered version", b, flip(0), aad},
		{"tampered nonce", b, flip(3), aad},
		{"tampered body", b, flip(len(ct) - 20), aad},
		{"tampered tag", b, flip(len(ct) - 1), aad},
		{"truncated", b, ct[:len(ct)-1], aad},
		{"too short", b, ct[:10], aad},
		{"empty", b, nil, aad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.box.Open(tt.ct, tt.aad); !errors.Is(err, secretbox.ErrDecrypt) {
				t.Fatalf("Open() err = %v, want ErrDecrypt", err)
			}
		})
	}
}
