package account

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	const good = "waka_good"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/api/v1/users/current/statusbar/today":
			w.WriteHeader(http.StatusNotFound)
		case r.Header.Get("Authorization") == "Basic "+base64.StdEncoding.EncodeToString([]byte("waka_boom")):
			w.WriteHeader(http.StatusBadGateway)
		case r.Header.Get("Authorization") == "Basic "+base64.StdEncoding.EncodeToString([]byte("waka_spa")):
			// A static site answers every path with 200 and its HTML shell.
			_, _ = w.Write([]byte(`<!doctype html><title>Logbook</title>`))
		case r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(good)):
			w.WriteHeader(http.StatusUnauthorized)
		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	defer srv.Close()
	tests := []struct {
		name, url, key string
		wantErr        error
		anyErr         bool
	}{
		{"valid", srv.URL + "/", good, nil, false},
		{"rejected", srv.URL, "waka_bad", ErrKeyRejected, true},
		{"empty", srv.URL, " ", ErrKeyRejected, true},
		{"server error", srv.URL, "waka_boom", nil, true},
		{"website, not the API", srv.URL, "waka_spa", ErrNotAPI, true},
		{"unreachable", "http://127.0.0.1:1", good, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Verify(context.Background(), &http.Client{Timeout: 2 * time.Second}, tt.url, tt.key, "test")
			if (err != nil) != tt.anyErr || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("Verify err = %v", err)
			}
			if tt.name == "valid" && time.Since(v.ServerTime) > time.Minute {
				t.Fatalf("server time = %v", v.ServerTime)
			}
		})
	}
}
