package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
)

// TestDaemonCrediting checks the wiring: the daemon generates the device
// key, registers it with the server's API key, and status shows the device.
func TestDaemonCrediting(t *testing.T) {
	e := newEnv(t)
	var (
		mu         sync.Mutex
		registered []map[string]any
		auth       string
	)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/users/current/heartbeats.bulk", e.logbook)
	mux.HandleFunc("POST /api/v1/devices", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		registered, auth = append(registered, body), r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e.cfg.ServerURL = srv.URL
	if err := config.Save(e.dir, e.cfg); err != nil {
		t.Fatal(err)
	}
	e.start(t, func(o *Options) {
		o.noCrediting, o.creditingInterval, o.DeviceName = false, 20*time.Millisecond, "test-laptop"
	})
	waitFor(t, "device registration in status", func() bool {
		return e.localStatus(t).Crediting.Device.ID == "0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b"
	})
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer "+logbookKey || registered[0]["name"] != "test-laptop" || registered[0]["os"] != runtime.GOOS {
		t.Fatalf("registration = %v with %q", registered[0], auth)
	}
	fi, err := os.Stat(config.DeviceKeyPath(e.dir))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("device key mode = %v", fi.Mode().Perm())
	}
	if s := e.localStatus(t); s.Crediting.Minutes.LastComputed.IsZero() {
		t.Fatalf("crediting never ran: %+v", s.Crediting)
	}
}
