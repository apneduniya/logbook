package ledger_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/devices"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/pkg/evidence"
)

var now = time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)

type env struct {
	t   *testing.T
	st  *store.Store
	mux *http.ServeMux
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := storetest.New(t)
	mux := http.NewServeMux()
	ledger.NewHandler(ledger.NewService(st, ledger.WithClock(func() time.Time { return now })), httpapitest.StubSession).Register(mux)
	return &env{t: t, st: st, mux: mux}
}

// device is a registered device and its private key.
type device struct {
	user uuid.UUID
	id   uuid.UUID
	priv ed25519.PrivateKey
}

// registered is when e.device registers devices, before every test day.
var registered = time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)

func (e *env) device(user uuid.UUID) device {
	e.t.Helper()
	return e.deviceAt(user, registered)
}

func (e *env) deviceAt(user uuid.UUID, at time.Time) device {
	e.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := devices.NewService(e.st, devices.WithClock(func() time.Time { return at })).Register(context.Background(), user,
		devices.Registration{Name: "laptop", OS: "linux", PublicKey: pub})
	if err != nil {
		e.t.Fatal(err)
	}
	return device{user: user, id: out.ID, priv: priv}
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// day builds a validly signed upload for d.
func (d device) day(date, prev, content string) ledger.Upload {
	return d.dayWithRules(date, prev, content, "v1")
}

func (d device) dayWithRules(date, prev, content, rules string) ledger.Upload {
	ev := evidence.Day{DeviceID: d.id.String(), Day: date, PrevChainHash: prev, ContentHash: contentHash(content), RulesVersion: rules}
	chain, sig, err := evidence.Sign(d.priv, ev)
	if err != nil {
		panic(err)
	}
	return ledger.Upload{
		Day: date, PrevChainHash: prev, ContentHash: ev.ContentHash, ChainHash: chain,
		Signature: base64.StdEncoding.EncodeToString(sig), RulesVersion: rules,
		EventCounts: map[string]int{"activity": 120, "heartbeat": 40},
	}
}

func (e *env) put(user, deviceID uuid.UUID, u ledger.Upload) (int, string) {
	e.t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		e.t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httpapitest.Request(e.t, http.MethodPost, "/api/v1/devices/"+deviceID.String()+"/evidence-days", user, string(b)))
	return rec.Code, rec.Body.String()
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestChainOfDaysWithGap(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))

	d1 := d.day("2026-11-16", "", "day one")
	if code, body := e.put(d.user, d.id, d1); code != http.StatusCreated {
		t.Fatalf("first day = %d %s", code, body)
	}
	d2 := d.day("2026-11-17", d1.ChainHash, "day two")
	if code, body := e.put(d.user, d.id, d2); code != http.StatusCreated {
		t.Fatalf("second day = %d %s", code, body)
	}
	// The agent was off on the 18th: the 19th links to the 17th.
	d4 := d.day("2026-11-19", d2.ChainHash, "day four")
	if code, body := e.put(d.user, d.id, d4); code != http.StatusCreated {
		t.Fatalf("day after gap = %d %s", code, body)
	}
	// Retrying an identical upload is fine.
	if code, body := e.put(d.user, d.id, d2); code != http.StatusOK {
		t.Fatalf("identical re-upload = %d %s", code, body)
	}
	// Filling the gap later would break the stored chain.
	if code, body := e.put(d.user, d.id, d.day("2026-11-18", d2.ChainHash, "late")); code != http.StatusConflict || !strings.Contains(body, "chain mismatch") {
		t.Fatalf("late gap fill = %d %s", code, body)
	}
	if n := e.count(`SELECT count(*) FROM evidence_days WHERE device_id = $1`, d.id); n != 3 {
		t.Fatalf("stored days = %d", n)
	}
}

func TestChainMismatch(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))
	d1 := d.day("2026-11-16", "", "one")
	if code, _ := e.put(d.user, d.id, d1); code != http.StatusCreated {
		t.Fatal("seed failed")
	}
	tests := []struct {
		name string
		up   ledger.Upload
	}{
		{"first day claims no predecessor again", d.day("2026-11-17", "", "two")},
		{"links to an unknown hash", d.day("2026-11-17", contentHash("x"), "two")},
		{"a day before the first links to nothing stored", d.day("2026-11-15", contentHash("x"), "zero")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := e.put(d.user, d.id, tc.up); code != http.StatusConflict || !strings.Contains(body, "chain mismatch") {
				t.Fatalf("status = %d %s, want 409 chain mismatch", code, body)
			}
		})
	}
}

func TestConflictingDayIsRejectedAndAudited(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	d := e.device(alice)
	if code, _ := e.put(d.user, d.id, d.day("2026-11-16", "", "original")); code != http.StatusCreated {
		t.Fatal("seed failed")
	}
	other := d.day("2026-11-16", "", "rewritten history")
	if code, body := e.put(d.user, d.id, other); code != http.StatusConflict {
		t.Fatalf("conflict = %d %s, want 409", code, body)
	}
	// Different event counts alone are a conflict too.
	counts := d.day("2026-11-16", "", "original")
	counts.EventCounts["activity"]++
	if code, _ := e.put(d.user, d.id, counts); code != http.StatusConflict {
		t.Fatalf("changed counts = %d, want 409", code)
	}
	audit := seedtest.Audit(t, e.st, alice)
	var conflicts []seedtest.AuditEntry
	for _, a := range audit {
		if a.Action == "evidence_conflict" {
			conflicts = append(conflicts, a)
		}
	}
	if len(conflicts) != 2 || conflicts[0].SubjectID != d.id.String() || !strings.Contains(conflicts[0].Data, other.ChainHash) {
		t.Fatalf("audit = %+v", audit)
	}
	if n := e.count(`SELECT count(*) FROM evidence_days WHERE content_hash = $1`, contentHash("original")); n != 1 {
		t.Fatal("original record was replaced")
	}
}

func TestBadSignatureAndHash(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	d := e.device(alice)
	stranger := e.device(alice) // another device of the same user

	good := d.day("2026-11-16", "", "x")
	wrongKey := stranger.day("2026-11-16", "", "x")
	wrongKey.ChainHash = good.ChainHash // right hash, signed by another key
	tampered := good
	tampered.ContentHash = contentHash("y") // signature no longer matches
	badHash := good
	badHash.ChainHash = contentHash("not the chain hash")
	badB64 := good
	badB64.Signature = "%%%"

	tests := []struct {
		name string
		up   ledger.Upload
	}{
		{"signed by another device", wrongKey},
		{"content changed after signing", tampered},
		{"chain hash does not match fields", badHash},
		{"signature not base64", badB64},
		{"bad day", func() ledger.Upload { u := good; u.Day = "2026-13-01"; return u }()},
		{"future day", d.day("2026-11-21", "", "x")},
		{"upper-case hex", func() ledger.Upload { u := good; u.ContentHash = strings.ToUpper(u.ContentHash); return u }()},
		{"bad event count key", func() ledger.Upload {
			u := good
			u.EventCounts = map[string]int{"/home/me/secret": 1}
			return u
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := e.put(d.user, d.id, tc.up); code != http.StatusBadRequest {
				t.Fatalf("status = %d %s, want 400", code, body)
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM evidence_days`); n != 0 {
		t.Fatalf("stored %d days", n)
	}
	// Today is accepted (the day being closed by a clock slightly ahead).
	if code, body := e.put(d.user, d.id, d.day("2026-11-20", "", "today")); code != http.StatusCreated {
		t.Fatalf("today = %d %s", code, body)
	}
}

func TestDeviceOwnershipAndRevocation(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	bob := seedtest.User(t, e.st, "bob", "UTC", true)
	d := e.device(alice)
	up := d.day("2026-11-16", "", "x")
	if code, _ := e.put(bob, d.id, up); code != http.StatusNotFound {
		t.Fatalf("bob = %d, want 404", code)
	}
	if code, _ := e.put(alice, uuid.New(), up); code != http.StatusNotFound {
		t.Fatalf("unknown device = %d, want 404", code)
	}
	if err := devices.NewService(e.st).Revoke(context.Background(), alice, d.id); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.put(alice, d.id, up); code != http.StatusForbidden {
		t.Fatalf("revoked = %d, want 403", code)
	}
	if code, _ := e.put(uuid.Nil, d.id, up); code != http.StatusUnauthorized {
		t.Fatalf("no auth = %d, want 401", code)
	}
}

// A key can't sign days from before it was registered: a fresh device must
// not be able to write a whole history for closed days.
func TestDaysBeforeRegistrationAreRejected(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	d := e.deviceAt(alice, time.Date(2026, 11, 18, 23, 50, 0, 0, time.UTC))
	for _, day := range []string{"2026-11-17", "2025-06-01"} {
		if code, body := e.put(alice, d.id, d.day(day, "", "made up")); code != http.StatusBadRequest || !strings.Contains(body, "registered") {
			t.Fatalf("%s = %d %s, want 400", day, code, body)
		}
	}
	// The registration day itself is fine (the agent starts its chain there).
	if code, body := e.put(alice, d.id, d.day("2026-11-18", "", "first")); code != http.StatusCreated {
		t.Fatalf("registration day = %d %s", code, body)
	}
}

func TestUnknownRulesVersionIsRejected(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))
	if code, body := e.put(d.user, d.id, d.dayWithRules("2026-11-16", "", "x", "v9")); code != http.StatusBadRequest || !strings.Contains(body, "rules_version") {
		t.Fatalf("status = %d %s, want 400", code, body)
	}
}
