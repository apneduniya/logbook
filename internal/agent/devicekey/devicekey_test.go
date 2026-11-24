package devicekey

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/pkg/evidence"
)

func TestKeyCreateLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.key")
	if _, err := Load(path); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Load(missing) = %v, want ErrNoKey", err)
	}
	k, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("LoadOrCreate = %v, %v", created, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("key mode = %v, %v; want 0600", fi.Mode().Perm(), err)
		}
	}
	b, _ := os.ReadFile(path)
	if !bytes.HasPrefix(b, []byte("-----BEGIN PRIVATE KEY-----")) {
		t.Fatalf("key file is not PKCS#8 PEM: %q", b[:20])
	}
	again, created, err := LoadOrCreate(path)
	if err != nil || created || !again.Public().Equal(k.Public()) {
		t.Fatalf("reload = %v, %v; same key %v", created, err, again.Public().Equal(k.Public()))
	}
	if _, err := Create(path); err == nil {
		t.Fatal("Create overwrote an existing key")
	}

	d := evidence.Day{DeviceID: "0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b", Day: "2026-11-19",
		ContentHash: strings.Repeat("ab", 32), RulesVersion: "v1", SeqTo: 12, Buckets: 1}
	ch, sig, err := k.Sign(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.Verify(again.Public(), d, ch, sig); err != nil {
		t.Fatalf("Verify = %v", err)
	}
}

func TestKeyNeverPrinted(t *testing.T) {
	k, err := Create(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "key", k)
	secret := base64.StdEncoding.EncodeToString(k.priv.Seed())
	seedBytes := fmt.Sprintf("%v", k.priv.Seed()[:8])
	seedBytes = strings.TrimSuffix(seedBytes, "]") // a prefix of how %v prints the bytes
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s", k, k, k, k), logs.String()} {
		if strings.Contains(s, secret) || strings.Contains(s, seedBytes) || !strings.Contains(s, "redacted") {
			t.Fatalf("key leaked or not redacted: %s", s)
		}
	}
}

func TestLoadTightensPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are advisory on Windows; the data dir ACL protects the key")
	}
	path := filepath.Join(t.TempDir(), "device.key")
	if _, err := Create(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.key")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(path); err == nil || errors.Is(err, ErrNoKey) {
		t.Fatalf("LoadOrCreate(garbage) = %v; a damaged key must not be silently replaced", err)
	}
}

// fakeServer implements POST /api/v1/devices per the contract: same user
// and key → 200 with the same id, a key of another user → 409.
type fakeServer struct {
	mu    sync.Mutex
	owner map[string]string // public key → api key
	ids   map[string]string // public key → id
	calls int
	// full refuses new keys with the active-device limit 409.
	full bool
	// takeNext answers the next takeNext new keys "registered to another
	// account".
	takeNext int
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.URL.Path != "/api/v1/devices" || user == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var req struct {
		Name, OS  string
		PublicKey []byte `json:"public_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.PublicKey) != ed25519.PublicKeySize {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
		return
	}
	pk := string(req.PublicKey)
	if owner, ok := f.owner[pk]; ok {
		if owner != user {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"public key is registered to another account"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":%q}`, f.ids[pk])
		return
	}
	if f.takeNext > 0 {
		f.takeNext--
		f.owner[pk] = "someone-else"
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"public key is registered to another account"}`))
		return
	}
	if f.full {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"you already have 20 active devices; revoke one first"}`))
		return
	}
	f.owner[pk] = user
	f.ids[pk] = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.ids)+1)
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, `{"id":%q}`, f.ids[pk])
}

type identityEnv struct {
	dir string
	db  *localdb.DB
	srv *fakeServer
	url string
	id  *Identity
	now time.Time
}

func newIdentityEnv(t *testing.T) *identityEnv {
	t.Helper()
	e := &identityEnv{dir: t.TempDir(), srv: &fakeServer{owner: map[string]string{}, ids: map[string]string{}},
		now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	db, err := localdb.Open(t.Context(), filepath.Join(e.dir, "logbook.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e.db = db
	ts := httptest.NewServer(e.srv)
	t.Cleanup(ts.Close)
	e.url = ts.URL
	e.id = NewIdentity(Options{KeyPath: filepath.Join(e.dir, "device.key"), Store: db,
		Registrar: serverapi.New(ts.Client(), "test"), Name: strings.Repeat("n", 100), OS: "linux",
		Now: func() time.Time { e.now = e.now.Add(time.Second); return e.now }})
	return e
}

func (e *identityEnv) ensure(t *testing.T, key string) (Device, error) {
	t.Helper()
	return e.id.Ensure(context.Background(), serverapi.Target{BaseURL: e.url, APIKey: key})
}

func TestEnsureRegistersOnceAndIsIdempotent(t *testing.T) {
	e := newIdentityEnv(t)
	d1, err := e.ensure(t, "user-a")
	if err != nil || d1.ID == "" || d1.Replaced || d1.RegisteredAt.IsZero() {
		t.Fatalf("Ensure = %+v, %v", d1, err)
	}
	d2, err := e.ensure(t, "user-a")
	if err != nil || d2.ID != d1.ID || e.srv.calls != 1 {
		t.Fatalf("second Ensure = %+v, %v (calls %d); want the stored id without a request", d2, err, e.srv.calls)
	}
	if v, _ := e.db.Get(context.Background(), KeyID); v != d1.ID {
		t.Fatalf("stored id = %q", v)
	}
	// The server forgot the device: registering the same key again is
	// idempotent and returns the same id.
	if err := e.id.Forget(context.Background()); err != nil {
		t.Fatal(err)
	}
	d3, err := e.ensure(t, "user-a")
	if err != nil || d3.ID != d1.ID || d3.Replaced || e.srv.calls != 2 {
		t.Fatalf("re-register = %+v, %v (calls %d)", d3, err, e.srv.calls)
	}
}

func TestEnsureNewServerReRegisters(t *testing.T) {
	e := newIdentityEnv(t)
	d1, err := e.ensure(t, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	other := &fakeServer{owner: map[string]string{}, ids: map[string]string{"x": "x"}}
	ts := httptest.NewServer(other)
	defer ts.Close()
	d2, err := e.id.Ensure(context.Background(), serverapi.Target{BaseURL: ts.URL, APIKey: "user-a"})
	if err != nil || d2.ID == d1.ID || !d2.Replaced || !d2.Key.Public().Equal(d1.Key.Public()) {
		t.Fatalf("Ensure on a new server = %+v, %v", d2, err)
	}
}

func TestEnsureKeyOfAnotherAccountRotates(t *testing.T) {
	e := newIdentityEnv(t)
	d1, err := e.ensure(t, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.id.Forget(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The account switch: an upload under user-b answered 404 for user-a's
	// device id (Forget), then the old key is refused with 409. The new
	// device replaces the old one even though Forget cleared the id first.
	d2, err := e.ensure(t, "user-b")
	if err != nil || d2.ID == d1.ID || d2.Key.Public().Equal(d1.Key.Public()) || !d2.Replaced {
		t.Fatalf("Ensure as another user = %+v, %v", d2, err)
	}
	retired, _ := filepath.Glob(filepath.Join(e.dir, "device.key.retired-*"))
	if len(retired) != 1 {
		t.Fatalf("retired keys = %v", retired)
	}
	if d3, err := e.ensure(t, "user-b"); err != nil || d3.ID != d2.ID || d3.Replaced {
		t.Fatalf("next Ensure = %+v, %v", d3, err)
	}
}

func TestDeviceLimitKeepsTheKey(t *testing.T) {
	e := newIdentityEnv(t)
	e.srv.full = true
	var first []byte
	for i := range 5 {
		_, err := e.ensure(t, "user-a")
		if serverapi.KindOf(err) != serverapi.KindConflict || serverapi.IsKeyTaken(err) {
			t.Fatalf("Ensure %d = %v, want the device-limit conflict", i, err)
		}
		b, rerr := os.ReadFile(filepath.Join(e.dir, "device.key"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if first == nil {
			first = b
		} else if !bytes.Equal(b, first) {
			t.Fatalf("Ensure %d replaced the key after a device-limit refusal", i)
		}
	}
	if retired, _ := filepath.Glob(filepath.Join(e.dir, "device.key.retired-*")); len(retired) != 0 {
		t.Fatalf("retired keys = %v; a device-limit refusal must not retire the key", retired)
	}
	// Once the user revokes another device, the same key registers.
	e.srv.full = false
	if d, err := e.ensure(t, "user-a"); err != nil || d.Replaced {
		t.Fatalf("Ensure after the limit cleared = %+v, %v", d, err)
	}
}

func TestRetireNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.key")
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var pubs []ed25519.PublicKey
	for range 3 {
		k, err := Create(path)
		if err != nil {
			t.Fatal(err)
		}
		pubs = append(pubs, k.Public())
		if err := Retire(path, now); err != nil { // the same second every time
			t.Fatal(err)
		}
	}
	retired, _ := filepath.Glob(filepath.Join(dir, "device.key.retired-*"))
	if len(retired) != 3 {
		t.Fatalf("retired keys = %v, want 3", retired)
	}
	for _, p := range retired {
		k, err := Load(p)
		if err != nil {
			t.Fatalf("Load(%s) = %v", p, err)
		}
		found := false
		for _, pub := range pubs {
			found = found || k.Public().Equal(pub)
		}
		if !found {
			t.Fatalf("%s holds an unknown key", p)
		}
	}
	if err := Retire(path, now); err != nil {
		t.Fatalf("Retire(missing) = %v", err)
	}
	if retired, _ := filepath.Glob(filepath.Join(dir, "device.key.retired-*")); len(retired) != 3 {
		t.Fatalf("Retire of a missing key made a file: %v", retired)
	}
}

func TestRevokedThenKeyTakenInOneEnsureKeepsBothKeys(t *testing.T) {
	e := newIdentityEnv(t)
	if _, err := e.ensure(t, "user-a"); err != nil {
		t.Fatal(err)
	}
	if repaired, err := e.id.MarkRevoked(context.Background(), serverapi.Target{BaseURL: e.url, APIKey: "user-a"}); err != nil || repaired {
		t.Fatalf("MarkRevoked = %v, %v", repaired, err)
	}
	// A frozen clock: both retirements in one Ensure share a second.
	e.id.opt.Now = func() time.Time { return e.now }
	// The key made after the revocation is answered "registered to another
	// account", so one Ensure retires two keys.
	e.srv.mu.Lock()
	e.srv.takeNext = 1
	e.srv.mu.Unlock()
	d, err := e.ensure(t, "user-a-2")
	if err != nil || !d.Replaced {
		t.Fatalf("Ensure = %+v, %v", d, err)
	}
	retired, _ := filepath.Glob(filepath.Join(e.dir, "device.key.retired-*"))
	if len(retired) != 2 {
		t.Fatalf("retired keys = %v, want 2 (none overwritten)", retired)
	}
}

func TestRevokedAfterNewLoginRepairs(t *testing.T) {
	e := newIdentityEnv(t)
	d1, err := e.ensure(t, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The user revoked the device and ran `logbook login` (user-a-2)
	// before the next upload: the 403 arrives under a key that never
	// worked with this device. That is not a fresh revocation.
	repaired, err := e.id.MarkRevoked(ctx, serverapi.Target{BaseURL: e.url, APIKey: "user-a-2"})
	if err != nil || !repaired {
		t.Fatalf("MarkRevoked = %v, %v; want repaired", repaired, err)
	}
	d2, err := e.ensure(t, "user-a-2")
	if err != nil || d2.ID == d1.ID || !d2.Replaced || d2.Key.Public().Equal(d1.Key.Public()) {
		t.Fatalf("Ensure after repair = %+v, %v", d2, err)
	}

	// A key that has uploaded for this device: a 403 is a real revocation.
	if err := e.id.Confirm(ctx, serverapi.Target{BaseURL: e.url, APIKey: "user-a-3"}); err != nil {
		t.Fatal(err)
	}
	repaired, err = e.id.MarkRevoked(ctx, serverapi.Target{BaseURL: e.url, APIKey: "user-a-3"})
	if err != nil || repaired {
		t.Fatalf("MarkRevoked after Confirm = %v, %v; want a revocation", repaired, err)
	}
	if _, err := e.ensure(t, "user-a-3"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Ensure = %v, want ErrRevoked", err)
	}
}

func TestDeviceLabels(t *testing.T) {
	tests := []struct{ name, os, wantName, wantOS string }{
		{"", "linux", DefaultName, "linux"},
		{"   ", "", DefaultName, "unknown"},
		{"\x00\x01", "darwin", DefaultName, "darwin"},
		{" laptop\u202e ", "windows", "laptop", "windows"},
		{strings.Repeat("é", 70), strings.Repeat("o", 40), strings.Repeat("é", 64), strings.Repeat("o", 32)},
		{"bad\xffutf8", "linux", "badutf8", "linux"},
	}
	for _, tt := range tests {
		id := NewIdentity(Options{Name: tt.name, OS: tt.os})
		if id.opt.Name != tt.wantName || id.opt.OS != tt.wantOS {
			t.Errorf("NewIdentity(%q, %q) = %q, %q; want %q, %q", tt.name, tt.os, id.opt.Name, id.opt.OS, tt.wantName, tt.wantOS)
		}
	}
}

func TestRevokedStaysRevokedUntilKeyChanges(t *testing.T) {
	e := newIdentityEnv(t)
	d1, err := e.ensure(t, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if repaired, err := e.id.MarkRevoked(context.Background(), serverapi.Target{BaseURL: e.url, APIKey: "user-a"}); err != nil || repaired {
		t.Fatalf("MarkRevoked = %v, %v", repaired, err)
	}
	if _, err := e.ensure(t, "user-a"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Ensure after revoke = %v, want ErrRevoked", err)
	}
	st, _ := e.db.Prefix(context.Background(), "device.")
	if strings.Contains(fmt.Sprint(st), "user-a") {
		t.Fatalf("API key stored in clear: %v", st)
	}
	// `logbook login` with a new key: a new key pair, a new device.
	d2, err := e.ensure(t, "user-a-2")
	if err != nil || d2.ID == d1.ID || !d2.Replaced || d2.Key.Public().Equal(d1.Key.Public()) {
		t.Fatalf("Ensure after new login = %+v, %v", d2, err)
	}
	if _, err := e.ensure(t, "user-a-2"); err != nil {
		t.Fatalf("revocation not cleared: %v", err)
	}
}

func TestEnsureSurfacesServerErrors(t *testing.T) {
	e := newIdentityEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	_, err := e.id.Ensure(context.Background(), serverapi.Target{BaseURL: ts.URL, APIKey: "bad"})
	if serverapi.KindOf(err) != serverapi.KindAuth {
		t.Fatalf("err = %v, want KindAuth", err)
	}
	if v, _ := e.db.Get(context.Background(), KeyID); v != "" {
		t.Fatalf("id stored after a failed registration: %q", v)
	}
}
