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
	svc *ledger.Service
	mux *http.ServeMux
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := storetest.New(t)
	mux := http.NewServeMux()
	svc := ledger.NewService(st, ledger.WithClock(func() time.Time { return now }))
	ledger.NewHandler(svc, httpapitest.StubSession).Register(mux)
	return &env{t: t, st: st, svc: svc, mux: mux}
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

// block is the signed part of an upload: the chain link plus the block
// fields of ADR 0019.
type block struct {
	date, prev, content, rules string
	seqFrom, seqTo             int64
	buckets                    int
	timeFrom, timeTo           *time.Time
}

// span is a typical block's time span: three minute buckets between 09:00
// and 17:00 on date.
func span(date string) (from, to *time.Time) {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		panic(err)
	}
	f, t := d.Add(9*time.Hour), d.Add(17*time.Hour)
	return &f, &t
}

// day builds a validly signed upload for a block of rows (seqFrom, seqTo]
// with a repo bucket and three minute buckets.
func (d device) day(date, prev, content string, seqFrom, seqTo int64) ledger.Upload {
	from, to := span(date)
	return d.sign(block{date: date, prev: prev, content: content, rules: "v1",
		seqFrom: seqFrom, seqTo: seqTo, buckets: 4, timeFrom: from, timeTo: to})
}

func (d device) sign(b block) ledger.Upload {
	ev := evidence.Day{DeviceID: d.id.String(), Day: b.date, PrevChainHash: b.prev, ContentHash: contentHash(b.content),
		RulesVersion: b.rules, SeqFrom: b.seqFrom, SeqTo: b.seqTo, Buckets: b.buckets, TimeFrom: b.timeFrom, TimeTo: b.timeTo}
	chain, sig, err := evidence.Sign(d.priv, ev)
	if err != nil {
		panic(err)
	}
	return upload(ev, chain, sig)
}

// upload is the request body for a signed record, without checking it.
func upload(ev evidence.Day, chain string, sig []byte) ledger.Upload {
	seqFrom, seqTo, buckets := ev.SeqFrom, ev.SeqTo, int64(ev.Buckets)
	return ledger.Upload{
		Day: ev.Day, PrevChainHash: ev.PrevChainHash, ContentHash: ev.ContentHash, ChainHash: chain,
		Signature: base64.StdEncoding.EncodeToString(sig), RulesVersion: ev.RulesVersion,
		EventCounts: map[string]int{"activity": 120, "heartbeat": 40},
		SeqFrom:     &seqFrom, SeqTo: &seqTo, Buckets: &buckets, TimeFrom: ev.TimeFrom, TimeTo: ev.TimeTo,
	}
}

func (e *env) put(user, deviceID uuid.UUID, u ledger.Upload) (int, string) {
	e.t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.putRaw(user, deviceID, string(b))
}

func (e *env) putRaw(user, deviceID uuid.UUID, body string) (int, string) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httpapitest.Request(e.t, http.MethodPost, "/api/v1/devices/"+deviceID.String()+"/evidence-days", user, body))
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

	d1 := d.day("2026-11-16", "", "day one", 0, 100)
	if code, body := e.put(d.user, d.id, d1); code != http.StatusCreated {
		t.Fatalf("first day = %d %s", code, body)
	}
	d2 := d.day("2026-11-17", d1.ChainHash, "day two", 100, 250)
	if code, body := e.put(d.user, d.id, d2); code != http.StatusCreated {
		t.Fatalf("second day = %d %s", code, body)
	}
	// The agent was off on the 18th: the 19th links to the 17th.
	d4 := d.day("2026-11-19", d2.ChainHash, "day four", 250, 251)
	if code, body := e.put(d.user, d.id, d4); code != http.StatusCreated {
		t.Fatalf("day after gap = %d %s", code, body)
	}
	// Retrying an identical upload is fine.
	if code, body := e.put(d.user, d.id, d2); code != http.StatusOK {
		t.Fatalf("identical re-upload = %d %s", code, body)
	}
	// Filling the gap later would break the stored chain.
	if code, body := e.put(d.user, d.id, d.day("2026-11-18", d2.ChainHash, "late", 250, 251)); code != http.StatusConflict || !strings.Contains(body, "chain mismatch") {
		t.Fatalf("late gap fill = %d %s", code, body)
	}
	if n := e.count(`SELECT count(*) FROM evidence_days WHERE device_id = $1`, d.id); n != 3 {
		t.Fatalf("stored days = %d", n)
	}
	// The block fields are stored as signed.
	if n := e.count(`SELECT count(*) FROM evidence_days WHERE device_id = $1 AND day = '2026-11-17'
		AND seq_from = 100 AND seq_to = 250 AND buckets = 4
		AND time_from = '2026-11-17T09:00:00Z' AND time_to = '2026-11-17T17:00:00Z'`, d.id); n != 1 {
		t.Fatal("block fields of the 17th were not stored as signed")
	}
}

func TestChainMismatch(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))
	d1 := d.day("2026-11-16", "", "one", 0, 10)
	if code, _ := e.put(d.user, d.id, d1); code != http.StatusCreated {
		t.Fatal("seed failed")
	}
	tests := []struct {
		name string
		up   ledger.Upload
	}{
		{"first day claims no predecessor again", d.day("2026-11-17", "", "two", 10, 20)},
		{"links to an unknown hash", d.day("2026-11-17", contentHash("x"), "two", 10, 20)},
		{"a day before the first links to nothing stored", d.day("2026-11-15", contentHash("x"), "zero", 0, 10)},
		{"a day before the first claims no predecessor", d.day("2026-11-15", "", "zero", 0, 10)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := e.put(d.user, d.id, tc.up); code != http.StatusConflict || !strings.Contains(body, "chain mismatch") {
				t.Fatalf("status = %d %s, want 409 chain mismatch", code, body)
			}
		})
	}
}

// Every block starts where the device's previous one ended, so every logged
// row is sealed exactly once (ADR 0019 point 2).
func TestSeqContinuity(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))

	// A device that took over from another starts at the log's high-water
	// mark at the takeover, not at 0.
	d1 := d.day("2026-11-16", "", "one", 5000, 5100)
	if code, body := e.put(d.user, d.id, d1); code != http.StatusCreated {
		t.Fatalf("first day from a takeover baseline = %d %s", code, body)
	}
	tests := []struct {
		name    string
		seqFrom int64
	}{
		{"overlaps the previous block", 5099},
		{"skips rows after the previous block", 5101},
		{"starts again from zero", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := d.day("2026-11-17", d1.ChainHash, "two", tc.seqFrom, 5200)
			if code, body := e.put(d.user, d.id, up); code != http.StatusConflict || !strings.Contains(body, "seq_from") {
				t.Fatalf("status = %d %s, want 409 seq_from", code, body)
			}
		})
	}
	// A block of only a repo snapshot has one bucket and no time span.
	repoOnly := d.sign(block{date: "2026-11-17", prev: d1.ChainHash, content: "repos", rules: "v1", seqFrom: 5100, seqTo: 5103, buckets: 1})
	if code, body := e.put(d.user, d.id, repoOnly); code != http.StatusCreated {
		t.Fatalf("repo-only block = %d %s", code, body)
	}
	if n := e.count(`SELECT count(*) FROM evidence_days WHERE device_id = $1 AND day = '2026-11-17' AND time_from IS NULL AND time_to IS NULL`, d.id); n != 1 {
		t.Fatal("repo-only block stored with a time span")
	}
	if n := e.count(`SELECT count(*) FROM evidence_days WHERE device_id = $1`, d.id); n != 2 {
		t.Fatalf("stored days = %d", n)
	}
}

// A record stored before ADR 0019 (dev only) has no seq_to, so the next
// day's seq_from can't be checked against it; the chain hash still is.
func TestDayAfterARecordWithoutBlockFields(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))
	legacyChain := contentHash("legacy chain")
	if _, err := e.st.Pool.Exec(context.Background(), `
		INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version)
		VALUES ($1, '2026-11-16', '', repeat('a', 64), $2, decode(repeat('00', 64), 'hex'), 'v1')`, d.id, legacyChain); err != nil {
		t.Fatal(err)
	}
	if code, body := e.put(d.user, d.id, d.day("2026-11-17", legacyChain, "two", 42, 50)); code != http.StatusCreated {
		t.Fatalf("day after a legacy record = %d %s", code, body)
	}
}

func TestConflictingDayIsRejectedAndAudited(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	d := e.device(alice)
	if code, _ := e.put(d.user, d.id, d.day("2026-11-16", "", "original", 0, 10)); code != http.StatusCreated {
		t.Fatal("seed failed")
	}
	other := d.day("2026-11-16", "", "rewritten history", 0, 10)
	if code, body := e.put(d.user, d.id, other); code != http.StatusConflict {
		t.Fatalf("conflict = %d %s, want 409", code, body)
	}
	// Different event counts alone are a conflict too.
	counts := d.day("2026-11-16", "", "original", 0, 10)
	counts.EventCounts["activity"]++
	if code, _ := e.put(d.user, d.id, counts); code != http.StatusConflict {
		t.Fatalf("changed counts = %d, want 409", code)
	}
	// So is a different, validly signed seq range for the same content.
	longer := d.day("2026-11-16", "", "original", 0, 11)
	if code, _ := e.put(d.user, d.id, longer); code != http.StatusConflict {
		t.Fatalf("changed seq_to = %d, want 409", code)
	}
	audit := seedtest.Audit(t, e.st, alice)
	var conflicts []seedtest.AuditEntry
	for _, a := range audit {
		if a.Action == "evidence_conflict" {
			conflicts = append(conflicts, a)
		}
	}
	if len(conflicts) != 3 || conflicts[0].SubjectID != d.id.String() || !strings.Contains(conflicts[0].Data, other.ChainHash) {
		t.Fatalf("audit = %+v", audit)
	}
	if n := e.count(`SELECT count(*) FROM evidence_days WHERE content_hash = $1 AND seq_to = 10`, contentHash("original")); n != 1 {
		t.Fatal("original record was replaced")
	}
}

func TestBadSignatureAndHash(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	d := e.device(alice)
	stranger := e.device(alice) // another device of the same user

	good := d.day("2026-11-16", "", "x", 0, 10)
	wrongKey := stranger.day("2026-11-16", "", "x", 0, 10)
	wrongKey.ChainHash = good.ChainHash // right hash, signed by another key
	tampered := good
	tampered.ContentHash = contentHash("y") // signature no longer matches
	badHash := good
	badHash.ChainHash = contentHash("not the chain hash")
	badB64 := good
	badB64.Signature = "%%%"
	i64 := func(v int64) *int64 { return &v }
	at := func(s string) *time.Time {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			panic(err)
		}
		return &t
	}
	from, to := span("2026-11-16")

	tests := []struct {
		name string
		up   ledger.Upload
	}{
		{"signed by another device", wrongKey},
		{"content changed after signing", tampered},
		{"chain hash does not match fields", badHash},
		{"signature not base64", badB64},
		{"seq_to changed after signing", func() ledger.Upload { u := good; u.SeqTo = i64(11); return u }()},
		{"seq_from changed after signing", func() ledger.Upload { u := good; u.SeqFrom = i64(1); return u }()},
		{"buckets changed after signing", func() ledger.Upload { u := good; u.Buckets = i64(5); return u }()},
		{"time span changed after signing", func() ledger.Upload { u := good; u.TimeTo = at("2026-11-16T17:01:00Z"); return u }()},
		{"time span dropped after signing", func() ledger.Upload { u := good; u.TimeFrom, u.TimeTo = nil, nil; return u }()},
		{"seq_to not after seq_from", func() ledger.Upload { u := good; u.SeqFrom, u.SeqTo = i64(10), i64(10); return u }()},
		{"negative seq_from", func() ledger.Upload { u := good; u.SeqFrom = i64(-1); return u }()},
		{"no buckets", func() ledger.Upload { u := good; u.Buckets = i64(0); return u }()},
		{"too many buckets", func() ledger.Upload { u := good; u.Buckets = i64(ledger.MaxBuckets + 1); return u }()},
		{"more buckets than minutes in the span", func() ledger.Upload {
			u := good
			u.TimeFrom, u.TimeTo, u.Buckets = from, from, i64(3)
			return u
		}()},
		{"several buckets without a time span", func() ledger.Upload { u := good; u.TimeFrom, u.TimeTo = nil, nil; u.Buckets = i64(2); return u }()},
		{"only time_from", func() ledger.Upload { u := good; u.TimeTo = nil; return u }()},
		{"span ends before it starts", func() ledger.Upload { u := good; u.TimeFrom, u.TimeTo = to, from; return u }()},
		{"span not on whole minutes", func() ledger.Upload { u := good; u.TimeFrom = at("2026-11-16T09:00:30Z"); return u }()},
		{"span not in UTC", func() ledger.Upload { u := good; u.TimeFrom = at("2026-11-16T14:30:00+05:30"); return u }()},
		{"bad day", func() ledger.Upload { u := good; u.Day = "2026-13-01"; return u }()},
		{"future day", d.day("2026-11-21", "", "x", 0, 10)},
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
	if code, body := e.put(d.user, d.id, d.day("2026-11-20", "", "today", 0, 10)); code != http.StatusCreated {
		t.Fatalf("today = %d %s", code, body)
	}
}

// An agent from before ADR 0019 sends no block fields; it must update
// rather than get a signature error it can't act on.
func TestOutdatedAgentIsRefused(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))
	sig := base64.StdEncoding.EncodeToString(make([]byte, 64))
	v1 := `{"day":"2026-11-16","prev_chain_hash":"","content_hash":"` + contentHash("x") + `","chain_hash":"` + contentHash("y") +
		`","signature":"` + sig + `","rules_version":"v1","event_counts":{"activity":1}`
	for name, body := range map[string]string{
		"no block fields":   v1 + `}`,
		"null seq_to":       v1 + `,"seq_from":0,"seq_to":null,"buckets":1}`,
		"no buckets":        v1 + `,"seq_from":0,"seq_to":5}`,
		"no seq_from":       v1 + `,"seq_to":5,"buckets":1}`,
		"only a time span":  v1 + `,"time_from":"2026-11-16T09:00:00Z","time_to":"2026-11-16T09:00:00Z"}`,
		"all fields absent": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, resp := e.putRaw(d.user, d.id, body)
			if code != http.StatusBadRequest || resp != `{"error":"update the Logbook agent"}`+"\n" {
				t.Fatalf("status = %d %q, want 400 update the Logbook agent", code, resp)
			}
		})
	}
}

func TestDeviceOwnershipAndRevocation(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	bob := seedtest.User(t, e.st, "bob", "UTC", true)
	d := e.device(alice)
	up := d.day("2026-11-16", "", "x", 0, 10)
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
	if n := e.count(`SELECT count(*) FROM evidence_days`); n != 0 {
		t.Fatalf("stored %d days", n)
	}
}

// A key can't sign days from before it was registered: a fresh device must
// not be able to write a whole history for closed days.
func TestDaysBeforeRegistrationAreRejected(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	d := e.deviceAt(alice, time.Date(2026, 11, 18, 23, 50, 0, 0, time.UTC))
	for _, day := range []string{"2026-11-17", "2025-06-01"} {
		if code, body := e.put(alice, d.id, d.day(day, "", "made up", 0, 10)); code != http.StatusBadRequest || !strings.Contains(body, "registered") {
			t.Fatalf("%s = %d %s, want 400", day, code, body)
		}
	}
	// The registration day itself is fine (the agent starts its chain there).
	if code, body := e.put(alice, d.id, d.day("2026-11-18", "", "first", 0, 10)); code != http.StatusCreated {
		t.Fatalf("registration day = %d %s", code, body)
	}
}

func TestUnknownRulesVersionIsRejected(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))
	from, to := span("2026-11-16")
	up := d.sign(block{date: "2026-11-16", content: "x", rules: "v9", seqTo: 10, buckets: 2, timeFrom: from, timeTo: to})
	if code, body := e.put(d.user, d.id, up); code != http.StatusBadRequest || !strings.Contains(body, "rules_version") {
		t.Fatalf("status = %d %s, want 400", code, body)
	}
}

// Days returns what was signed, so the submission re-check can verify
// proofs against it, and only for the caller's own device.
func TestDaysReadsTheSignedRecords(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	bob := seedtest.User(t, e.st, "bob", "UTC", true)
	d := e.device(alice)
	other := e.device(alice)

	d1 := d.day("2026-11-16", "", "one", 0, 10)
	d2 := d.sign(block{date: "2026-11-17", prev: d1.ChainHash, content: "repos", rules: "v1", seqFrom: 10, seqTo: 12, buckets: 1})
	d3 := d.day("2026-11-19", d2.ChainHash, "three", 12, 40)
	for _, up := range []ledger.Upload{d1, d2, d3, other.day("2026-11-17", "", "other device", 0, 5)} {
		dev := d.id
		if up.ContentHash == contentHash("other device") {
			dev = other.id
		}
		if code, body := e.put(alice, dev, up); code != http.StatusCreated {
			t.Fatalf("seed %s = %d %s", up.Day, code, body)
		}
	}

	from := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC) // the time of day is ignored
	got, err := e.svc.Days(ctx, alice, d.id, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Record.Day != "2026-11-17" || got[1].Record.Day != "2026-11-19" {
		t.Fatalf("days = %+v", got)
	}
	pub := d.priv.Public().(ed25519.PublicKey)
	for i, want := range []ledger.Upload{d2, d3} {
		s := got[i]
		if !s.HasBlock || s.ChainHash != want.ChainHash || s.Record.SeqFrom != *want.SeqFrom || s.Record.SeqTo != *want.SeqTo ||
			int64(s.Record.Buckets) != *want.Buckets || s.EventCounts["activity"] != 120 {
			t.Fatalf("day %d = %+v, want %+v", i, s, want)
		}
		if err := evidence.Verify(pub, s.Record, s.ChainHash, s.Signature); err != nil {
			t.Fatalf("stored %s no longer verifies: %v", s.Record.Day, err)
		}
	}
	if got[0].Record.TimeFrom != nil || got[1].Record.TimeFrom == nil || !got[1].Record.TimeFrom.Equal(*d3.TimeFrom) ||
		got[1].Record.TimeFrom.Location() != time.UTC {
		t.Fatalf("time spans = %v, %v", got[0].Record.TimeFrom, got[1].Record.TimeFrom)
	}

	// Another user can't read alice's evidence, even with the device id.
	if got, err := e.svc.Days(ctx, bob, d.id, from, to); err != nil || len(got) != 0 {
		t.Fatalf("bob reading alice's device = %+v, %v", got, err)
	}
}

// A record stored before ADR 0019 is read back without block fields.
func TestDaysMarksRecordsWithoutBlockFields(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	d := e.device(alice)
	if _, err := e.st.Pool.Exec(context.Background(), `
		INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version)
		VALUES ($1, '2026-11-16', '', repeat('a', 64), repeat('b', 64), decode(repeat('00', 64), 'hex'), 'v1')`, d.id); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	got, err := e.svc.Days(context.Background(), alice, d.id, day, day)
	if err != nil || len(got) != 1 || got[0].HasBlock || got[0].Record.SeqTo != 0 {
		t.Fatalf("days = %+v, %v", got, err)
	}
}

// The table refuses block fields that evidence.Day.Validate would, so no
// code path can store a record the re-check can't use.
func TestTableChecksBlockFields(t *testing.T) {
	e := newEnv(t)
	d := e.device(seedtest.User(t, e.st, "alice", "UTC", true))
	for name, cols := range map[string]string{
		"only seq_to":               `NULL, 5, NULL, NULL, NULL`,
		"seq_to not after seq_from": `5, 5, 1, NULL, NULL`,
		"negative seq_from":         `-1, 5, 1, NULL, NULL`,
		"no buckets":                `0, 5, 0, NULL, NULL`,
		"two buckets, no span":      `0, 5, 2, NULL, NULL`,
		"only time_from":            `0, 5, 2, '2026-11-16T09:00:00Z', NULL`,
		"span not on a minute":      `0, 5, 2, '2026-11-16T09:00:01Z', '2026-11-16T09:01:00Z'`,
		"span reversed":             `0, 5, 2, '2026-11-16T09:01:00Z', '2026-11-16T09:00:00Z'`,
		"span with buckets = 1":     `0, 5, 1, '2026-11-16T09:00:00Z', '2026-11-16T09:00:00Z'`,
		"more buckets than minutes": `0, 5, 3, '2026-11-16T09:00:00Z', '2026-11-16T09:00:00Z'`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.st.Pool.Exec(context.Background(), `
				INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version,
					seq_from, seq_to, buckets, time_from, time_to)
				VALUES ($1, '2026-11-16', '', repeat('a', 64), repeat('b', 64), decode(repeat('00', 64), 'hex'), 'v1', `+cols+`)`, d.id)
			if err == nil || !strings.Contains(err.Error(), "evidence_days_block_check") {
				t.Fatalf("insert = %v, want the block check to refuse it", err)
			}
		})
	}
	// The largest span check boundary: two buckets in one minute's span.
	if _, err := e.st.Pool.Exec(context.Background(), `
		INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version,
			seq_from, seq_to, buckets, time_from, time_to)
		VALUES ($1, '2026-11-16', '', repeat('a', 64), repeat('b', 64), decode(repeat('00', 64), 'hex'), 'v1',
			0, 5, 2, '2026-11-16T09:00:00Z', '2026-11-16T09:00:00Z')`, d.id); err != nil {
		t.Fatalf("valid block fields refused: %v", err)
	}
}
