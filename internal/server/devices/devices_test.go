package devices_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/devices"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

type env struct {
	t   *testing.T
	st  *store.Store
	mux *http.ServeMux
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := storetest.New(t)
	mux := http.NewServeMux()
	// The stub authenticates both API-key and session routes from a header.
	devices.NewHandler(devices.NewService(st), httpapitest.StubSession, httpapitest.StubSession).Register(mux)
	return &env{t: t, st: st, mux: mux}
}

func (e *env) do(method, target string, user uuid.UUID, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httpapitest.Request(e.t, method, target, user, body))
	return rec
}

func newKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func registerBody(name, key string) string {
	b, _ := json.Marshal(map[string]string{"name": name, "os": "linux", "public_key": key})
	return string(b)
}

func idOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.ID == "" {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return body.ID
}

func TestRegisterIsIdempotentPerUserAndKey(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	key := newKey(t)

	first := e.do(http.MethodPost, "/api/v1/devices", alice, registerBody("laptop", key))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	again := e.do(http.MethodPost, "/api/v1/devices", alice, registerBody("renamed", key))
	if again.Code != http.StatusOK || idOf(t, again) != idOf(t, first) {
		t.Fatalf("again = %d %s, want 200 with the same id", again.Code, again.Body)
	}
	audit := seedtest.Audit(t, e.st, alice)
	if len(audit) != 1 || audit[0].Action != "device.register" || audit[0].SubjectID != idOf(t, first) {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestRegisterKeyOfAnotherUserConflicts(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	bob := seedtest.User(t, e.st, "bob", "UTC", true)
	key := newKey(t)
	if rec := e.do(http.MethodPost, "/api/v1/devices", alice, registerBody("laptop", key)); rec.Code != http.StatusCreated {
		t.Fatalf("alice = %d", rec.Code)
	}
	rec := e.do(http.MethodPost, "/api/v1/devices", bob, registerBody("laptop", key))
	if rec.Code != http.StatusConflict {
		t.Fatalf("bob = %d %s, want 409", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "alice") {
		t.Fatal("conflict response names the other user")
	}
}

func TestConcurrentRegistrationsOfOneKey(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	key := newKey(t)
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Go(func() {
			rec := e.do(http.MethodPost, "/api/v1/devices", alice, registerBody("laptop", key))
			if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
				t.Errorf("status %d %s", rec.Code, rec.Body)
				return
			}
			ids[i] = idOf(t, rec)
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("ids differ: %v", ids)
		}
	}
}

func TestRegisterValidation(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	short := base64.StdEncoding.EncodeToString(make([]byte, 31))
	tests := []struct{ name, body string }{
		{"empty name", registerBody(" ", newKey(t))},
		{"long name", registerBody(strings.Repeat("n", 65), newKey(t))},
		{"control char", registerBody("lap\ttop", newKey(t))},
		{"bidi override", registerBody("lap\u202etop", newKey(t))},
		{"short key", registerBody("laptop", short)},
		{"url base64 key", registerBody("laptop", strings.NewReplacer("+", "-", "/", "_").Replace(newKey(t))+"x")},
		{"no os", `{"name":"laptop","os":"","public_key":"` + newKey(t) + `"}`},
		{"long os", `{"name":"laptop","os":"` + strings.Repeat("o", 33) + `","public_key":"` + newKey(t) + `"}`},
		{"not json", `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if rec := e.do(http.MethodPost, "/api/v1/devices", alice, tc.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body)
			}
		})
	}
	if rec := e.do(http.MethodPost, "/api/v1/devices", uuid.Nil, registerBody("laptop", newKey(t))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth = %d", rec.Code)
	}
}

func TestRegisterCap(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	for i := range devices.MaxActiveDevices {
		if rec := e.do(http.MethodPost, "/api/v1/devices", alice, registerBody(fmt.Sprint("d", i), newKey(t))); rec.Code != http.StatusCreated {
			t.Fatalf("device %d = %d", i, rec.Code)
		}
	}
	if rec := e.do(http.MethodPost, "/api/v1/devices", alice, registerBody("one more", newKey(t))); rec.Code != http.StatusConflict {
		t.Fatalf("over cap = %d, want 409", rec.Code)
	}
}

type listed struct {
	Devices []struct {
		ID             string  `json:"id"`
		Name           string  `json:"name"`
		PublicKey      string  `json:"public_key"`
		KeyFingerprint string  `json:"key_fingerprint"`
		RevokedAt      *string `json:"revoked_at"`
	} `json:"devices"`
}

func (e *env) list(user uuid.UUID) listed {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/v1/me/devices", user, "")
	if rec.Code != http.StatusOK {
		e.t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var l listed
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		e.t.Fatal(err)
	}
	return l
}

func TestListAndRevoke(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	bob := seedtest.User(t, e.st, "bob", "UTC", true)
	key := newKey(t)
	id := idOf(t, e.do(http.MethodPost, "/api/v1/devices", alice, registerBody("laptop", key)))
	seedtest.Device(t, e.st, bob, "bob's")

	l := e.list(alice)
	if len(l.Devices) != 1 || l.Devices[0].ID != id || l.Devices[0].PublicKey != key || len(l.Devices[0].KeyFingerprint) != 16 {
		t.Fatalf("alice's devices = %+v", l)
	}
	// Bob can neither see nor revoke Alice's device.
	if l := e.list(bob); len(l.Devices) != 1 || l.Devices[0].ID == id {
		t.Fatalf("bob's devices = %+v", l)
	}
	if rec := e.do(http.MethodDelete, "/api/v1/me/devices/"+id, bob, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob revoke = %d, want 404", rec.Code)
	}

	if rec := e.do(http.MethodDelete, "/api/v1/me/devices/"+id, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(http.MethodDelete, "/api/v1/me/devices/"+id, alice, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second revoke = %d, want 404", rec.Code)
	}
	if rec := e.do(http.MethodDelete, "/api/v1/me/devices/not-a-uuid", alice, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id = %d, want 404", rec.Code)
	}
	// Revoked devices stay listed, with their history.
	l = e.list(alice)
	if len(l.Devices) != 1 || l.Devices[0].RevokedAt == nil {
		t.Fatalf("after revoke = %+v", l)
	}
	// Registering the revoked key again doesn't revive it.
	rec := e.do(http.MethodPost, "/api/v1/devices", alice, registerBody("laptop", key))
	if rec.Code != http.StatusOK || idOf(t, rec) != id {
		t.Fatalf("re-register = %d %s", rec.Code, rec.Body)
	}
	if l := e.list(alice); l.Devices[0].RevokedAt == nil {
		t.Fatal("re-registering revived a revoked device")
	}
	audit := seedtest.Audit(t, e.st, alice)
	if len(audit) != 2 || audit[1].Action != "device.revoke" || audit[1].SubjectID != id {
		t.Fatalf("audit = %+v", audit)
	}
	if rec := e.do(http.MethodGet, "/api/v1/me/devices", uuid.Nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list without session = %d", rec.Code)
	}
}

func TestParseRegistration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	reg, err := devices.ParseRegistration("  My Laptop  ", " darwin ", key)
	if err != nil || reg.Name != "My Laptop" || reg.OS != "darwin" || len(reg.PublicKey) != 32 {
		t.Fatalf("ParseRegistration = %+v, %v", reg, err)
	}
	if got := devices.Fingerprint(make([]byte, 32)); got != "66687aadf862bd77" {
		t.Fatalf("Fingerprint = %q", got)
	}
}
