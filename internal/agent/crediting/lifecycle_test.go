package crediting

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// minuteRequests counts requests to the minutes endpoint.
func (f *fakeServer) minuteRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.requests {
		if strings.HasSuffix(p, "/minutes") {
			n++
		}
	}
	return n
}

func retiredKeys(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "device.key.retired-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUnknownRulesVersionHoldsMinutes(t *testing.T) {
	e := newEnv(t, t0)
	e.srv.set(func(f *fakeServer) { f.rulesVersion = "v999" })
	e.credit(at("11:58"), "app", 60)
	e.credit(at("11:59"), "app", 60)
	e.run()
	s := e.status()
	if s.Minutes.Refused == "" || !strings.Contains(s.Minutes.Refused, "rules_version") {
		t.Fatalf("refused = %q, want the server's reason", s.Minutes.Refused)
	}
	if s.Minutes.Waiting != 2 || s.Minutes.Rejected != 0 || s.Minutes.LastError != "" {
		t.Fatalf("status = %+v; minutes must be kept, not rejected, and uploads not backed off", s.Minutes)
	}
	if n := e.srv.minuteRequests(); n != 1 {
		t.Fatalf("minute requests = %d, want 1 (no splitting into single minutes)", n)
	}
	if !e.runner.NextAttempt().IsZero() {
		t.Fatal("a refused batch backed off the whole uploader")
	}

	// On hold: no more minute requests for a while.
	e.advance(time.Minute)
	e.run()
	if n := e.srv.minuteRequests(); n != 1 {
		t.Fatalf("minute requests while on hold = %d", n)
	}

	// The server learns the version (an update): the kept minutes go up.
	e.srv.set(func(f *fakeServer) { f.rulesVersion = struggle.RulesVersion })
	e.advance(31 * time.Minute)
	e.run()
	e.wantServerMinute(at("11:58"), "app", 60)
	e.wantServerMinute(at("11:59"), "app", 60)
	if s := e.status(); s.Minutes.Refused != "" || s.Minutes.Waiting != 0 {
		t.Fatalf("status after recovery = %+v", s.Minutes)
	}
}

// flakySource is an active-time source that fails while fail is set.
type flakySource struct{ fail *bool }

func (f flakySource) ActiveTimes(context.Context, time.Time, time.Time) ([]struggle.ActiveTime, error) {
	if *f.fail {
		return nil, errors.New("active time unreadable")
	}
	return nil, nil
}

func TestLocalSealErrorDoesNotBackOff(t *testing.T) {
	fail := true
	e := newEnv(t, time.Date(2026, 11, 17, 8, 0, 5, 0, time.UTC))
	e.active = flakySource{&fail}
	e.open()
	e.run() // registers on 2026-11-17
	id := e.deviceID()
	addDay(t, e.db, "2026-11-17")

	e.mu.Lock()
	e.now = time.Date(2026, 11, 18, 1, 30, 5, 0, time.UTC)
	e.mu.Unlock()
	e.credit(time.Date(2026, 11, 18, 1, 29, 0, 0, time.UTC), "app", 60)
	e.run()
	s := e.status()
	if s.Evidence.LocalError == "" || s.Minutes.LastError != "" || !e.runner.NextAttempt().IsZero() {
		t.Fatalf("status = %+v / %+v; a local sealing error must not back off uploads", s.Evidence, s.Minutes)
	}
	e.wantServerMinute(time.Date(2026, 11, 18, 1, 29, 0, 0, time.UTC), "app", 60)
	if days := e.srv.storedDays(id); len(days) != 0 {
		t.Fatalf("server days = %+v", days)
	}

	// The source recovers: the day is sealed on the next run.
	fail = false
	e.advance(time.Minute)
	e.run()
	if days := e.srv.storedDays(id); len(days) != 1 || days[0].Day != "2026-11-17" {
		t.Fatalf("server days = %+v", days)
	}
	if s := e.status(); s.Evidence.LocalError != "" || s.Evidence.LastDay != "2026-11-17" {
		t.Fatalf("evidence status = %+v", s.Evidence)
	}
}

func TestRevokedThenLoginBeforeNextUpload(t *testing.T) {
	e := newEnv(t, t0)
	e.credit(at("11:59"), "app", 60)
	e.run()
	old := e.deviceID()
	// While the agent was offline the user revoked the device and ran
	// `logbook login` with a new key of the same account.
	e.srv.set(func(f *fakeServer) { f.revoked[old] = true })
	e.setKey("key-1b")
	e.credit(at("12:00"), "app", 60)
	e.advance(time.Minute)
	e.run() // 403 under a key that never worked with this device
	if s := e.status(); !s.Device.RevokedAt.IsZero() || s.Minutes.Paused == reasonRevoked {
		t.Fatalf("status = %+v; the new login must not be told to log in again", s)
	}
	e.advance(2 * time.Second)
	e.run() // registers a new device
	id := e.deviceID()
	if id == old || len(retiredKeys(t, e.dir)) != 1 {
		t.Fatalf("device id = %s (old %s), retired keys %v", id, old, retiredKeys(t, e.dir))
	}
	e.credit(at("12:02"), "app", 60)
	e.advance(2 * time.Minute) // 12:03:07: 12:02 has ended
	e.run()
	e.wantServerMinute(at("12:02"), "app", 60)
	if s := e.status(); s.Minutes.Paused != "" || s.Device.ID != id {
		t.Fatalf("status = %+v", s)
	}
}

func TestAccountSwitchDoesNotCarryMinutes(t *testing.T) {
	e := newEnv(t, t0)
	// Account u1: the device registers, but minutes can't be uploaded.
	e.srv.set(func(f *fakeServer) { f.status = http.StatusServiceUnavailable })
	e.credit(at("11:59"), "app", 60)
	e.run()
	old := e.deviceID()

	// The user logs in as another account (u2).
	e.setKey("key-2")
	e.srv.set(func(f *fakeServer) { f.status = 0 })
	e.advance(2 * time.Second)
	e.run() // 404: u1's device isn't u2's; forget the id
	e.advance(2 * time.Second)
	e.run() // the old key is u1's (409): a new key and device for u2
	id, err := e.db.Get(context.Background(), devicekey.KeyID)
	if err != nil || id == "" || id == old {
		t.Fatalf("device id = %q (old %s), %v", id, old, err)
	}
	if len(retiredKeys(t, e.dir)) != 1 {
		t.Fatalf("retired keys = %v", retiredKeys(t, e.dir))
	}
	for _, dev := range []string{old, id} {
		if m, ok := e.srv.minute(dev, at("11:59"), "app"); ok {
			t.Fatalf("u1's minute was uploaded under device %s: %+v", dev, m)
		}
	}
	if s := e.status(); s.Minutes.Waiting != 0 {
		t.Fatalf("waiting = %d; u1's minutes must be retired, not queued for u2", s.Minutes.Waiting)
	}
}

func TestMinuteNeverSplitAcrossBatches(t *testing.T) {
	e := newEnv(t, t0)
	e.options = func(o *Options) { o.Batch = 1 }
	e.open()
	e.credit(at("11:57"), "app", 60)
	e.creditSplit(at("11:58"), map[string]int{"alpha": 10, "zeta": 50})
	e.run()
	e.wantServerMinute(at("11:58"), "alpha", 10)
	e.wantServerMinute(at("11:58"), "zeta", 50)

	// Credit moves between the projects: zeta 50→30 and alpha 10→30. Sent
	// in separate batches, alpha's raise would hit the 60-second cap.
	e.creditSplit(at("11:58"), map[string]int{"alpha": 30, "zeta": 30})
	e.advance(time.Minute)
	e.run()
	e.wantServerMinute(at("11:58"), "alpha", 30)
	e.wantServerMinute(at("11:58"), "zeta", 30)
	if s := e.status(); s.Minutes.Rejected != 0 || s.Minutes.Waiting != 0 {
		t.Fatalf("status = %+v", s.Minutes)
	}
	for i, b := range e.srv.batches() {
		for _, m := range b[1:] {
			if !m.Minute.Equal(b[0].Minute) {
				t.Fatalf("batch %d mixes minutes with batch size 1: %+v", i, b)
			}
		}
		if b[0].Minute.Equal(at("11:58")) && len(b) != 2 {
			t.Fatalf("batch %d split minute 11:58: %+v", i, b)
		}
	}
}

func TestDeviceLimitShownAndKeyKept(t *testing.T) {
	e := newEnv(t, t0)
	e.srv.set(func(f *fakeServer) { f.deviceLimit = true })
	e.credit(at("11:59"), "app", 60)
	for range 3 {
		e.run()
		e.advance(2 * time.Second)
	}
	s := e.status()
	if !strings.Contains(s.Minutes.Paused, "active devices") || s.Device.ID != "" {
		t.Fatalf("status = %+v", s)
	}
	if r := retiredKeys(t, e.dir); len(r) != 0 {
		t.Fatalf("retired keys = %v; the device limit must not rotate the key", r)
	}
	// The user revokes an old device on the dashboard.
	e.srv.set(func(f *fakeServer) { f.deviceLimit = false })
	e.advance(time.Minute)
	e.run()
	e.wantServerMinute(at("11:59"), "app", 60)
	if s := e.status(); s.Minutes.Paused != "" {
		t.Fatalf("still paused: %q", s.Minutes.Paused)
	}
}
