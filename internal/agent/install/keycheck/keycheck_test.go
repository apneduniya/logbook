package keycheck

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const key = "3f1e2d4c-0000-4000-8000-000000000000"

func TestNormalize(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{key, key, true},
		{"  3F1E2D4C-0000-4000-8000-000000000000\n", key, true},
		{"waka_" + key, "", false},
		{"3f1e2d4c-0000-1000-8000-000000000000", "", false}, // not v4
		{"", "", false},
	}
	for _, tt := range tests {
		got, err := Normalize(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("Normalize(%q) = %q, %v", tt.in, got, err)
		}
		if !tt.ok && !errors.Is(err, ErrMalformed) {
			t.Errorf("Normalize(%q) err = %v, want ErrMalformed", tt.in, err)
		}
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/users/current/statusbar/today" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Basic " + base64.StdEncoding.EncodeToString([]byte(key)):
			_, _ = w.Write([]byte(`{"data":{}}`))
		case "Basic " + base64.StdEncoding.EncodeToString([]byte("boom")):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	c := New(srv.Client(), "logbook-test")
	ctx := context.Background()

	if err := c.Check(ctx, srv.URL+"/", key); err != nil {
		t.Fatalf("valid key: %v", err)
	}
	if err := c.Check(ctx, srv.URL, "other"); !errors.Is(err, ErrRejected) {
		t.Fatalf("bad key: %v, want ErrRejected", err)
	}
	if err := c.Check(ctx, srv.URL, "boom"); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("server error: %v", err)
	}
	for _, bad := range []string{"ftp://x", "http://logbook.example", "not a url"} {
		if err := c.Check(ctx, bad, key); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
