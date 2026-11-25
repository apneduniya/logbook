package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/admin"
	"github.com/apneduniya/logbook/internal/server/ledger"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/internal/server/submissions"
)

// testNow is every service's clock: the fixture's activity is on 10 June
// 2026.
var testNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func day(h, m int) time.Time { return time.Date(2026, 6, 10, h, m, 0, 0, time.UTC) }

// fixture is a database with an admin (fiona, holding key), the users
// admins look at, and the admin API's handler on mux.
type fixture struct {
	st   *store.Store
	keys *admin.Keys
	mux  *http.ServeMux
	key  string

	fiona, orpheus, pat, otto uuid.UUID
	orpheusID, patID          int64
	laptop                    uuid.UUID
}

// newFixture seeds:
//
//   - fiona, an admin with one key;
//   - orpheus (Hackatime 4242, GitHub orpheus-gh, now yellow after blue
//     and green): 40 heartbeats on logbook (Go, one a minute from 09:00,
//     the last 10 "ai coding"), 2 on site (Svelte, browsing), one on
//     secret (hidden project); a laptop with 20 credited minutes on
//     logbook and 5 on secret, and two sealed evidence days with a gap;
//     an API key;
//   - pat (unlinked, private), with no activity;
//   - otto, whose display name contains "orph" (for search).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := storetest.New(t)
	ctx := context.Background()
	clock := func() time.Time { return testNow }
	f := &fixture{st: st, keys: admin.NewKeys(st, clock), mux: http.NewServeMux()}
	f.fiona = seedtest.User(t, st, "fiona", "UTC", true)
	f.orpheus = seedtest.User(t, st, "orpheus", "America/New_York", true)
	f.pat = seedtest.User(t, st, "pat", "UTC", false)
	f.otto = seedtest.User(t, st, "otto", "UTC", true)
	f.orpheusID = seedtest.PublicID(t, st, f.orpheus)
	f.patID = seedtest.PublicID(t, st, f.pat)
	exec(t, st, `UPDATE users SET display_name = 'Otto Orphan' WHERE id = $1`, f.otto)

	if _, err := f.keys.Grant(ctx, "fiona"); err != nil {
		t.Fatal(err)
	}
	nk, err := f.keys.CreateKey(ctx, "fiona", "fraud squad laptop")
	if err != nil {
		t.Fatal(err)
	}
	f.key = nk.Key

	o := f.orpheus
	seedtest.HackatimeLink(t, st, o, 4242, "orpheus-gh", "yellow")
	for _, r := range []struct {
		level string
		at    time.Time
	}{
		{"blue", time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)},
		{"blue", time.Date(2026, 5, 2, 8, 0, 0, 0, time.UTC)},
		{"green", time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC)},
		{"yellow", time.Date(2026, 6, 15, 8, 0, 0, 0, time.UTC)},
		{"yellow", time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC)},
	} {
		if err := st.RecordHackatimeTrust(ctx, db.RecordHackatimeTrustParams{UserID: o, Level: r.level, CheckedAt: r.at}); err != nil {
			t.Fatal(err)
		}
	}
	seedtest.ProjectSettings(t, st, o, "logbook", "https://github.com/orpheus/logbook", false, false)
	seedtest.ProjectSettings(t, st, o, "site", "", false, true)
	seedtest.ProjectSettings(t, st, o, "secret", "", true, false)
	var hbs []seedtest.Heartbeat
	for i := range 40 {
		cat := "coding"
		if i >= 30 {
			cat = "ai coding"
		}
		hbs = append(hbs, seedtest.Heartbeat{At: day(9, i), Project: "logbook", Category: cat})
	}
	hbs = append(hbs,
		seedtest.Heartbeat{At: day(10, 0), Project: "site", Language: "Svelte", Category: "browsing", Entity: "https://example.com"},
		seedtest.Heartbeat{At: day(10, 0).Add(30 * time.Second), Project: "site", Language: "Svelte", Category: "browsing", Entity: "https://example.com"},
		seedtest.Heartbeat{At: day(11, 0), Project: "secret", Language: "Rust", Entity: "secret.rs"},
	)
	seedtest.Heartbeats(t, st, o, hbs...)
	f.laptop = seedtest.Device(t, st, o, "laptop")
	seedtest.CreditedSpan(t, st, o, f.laptop, day(9, 0), 20, seedtest.Credited{Project: "logbook", Seconds: 60})
	seedtest.CreditedSpan(t, st, o, f.laptop, day(11, 0), 5, seedtest.Credited{Project: "secret", Seconds: 60})
	for _, d := range []string{"2026-06-08", "2026-06-10"} {
		exec(t, st, `INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version)
			VALUES ($1, $2, '', repeat('a', 64), repeat('b', 64), decode(repeat('00', 64), 'hex'), 'v1')`, f.laptop, d)
	}
	exec(t, st, `INSERT INTO api_keys (user_id, name, key_hash, key_prefix, created_at) VALUES ($1, 'editor', $2, 'abcd1234', now())`,
		o, bytes.Repeat([]byte{7}, 32))

	checker := submissions.NewChecker(st, ledger.NewService(st), submissions.CheckerOptions{Now: clock})
	admin.NewHandler(admin.NewService(st, checker, clock), f.keys, admin.HandlerOptions{}).Register(f.mux)
	return f
}

// exec runs a fixture statement the seed helpers don't cover.
func exec(t *testing.T, st *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// call is one request: the fixture's admin key unless key or auth is set.
type call struct {
	method, target string
	key            string // "-" sends no Authorization header
	auth           string // a raw Authorization header
	body           string // JSON
	form           string // a form body
	cookie         *http.Cookie
}

func (f *fixture) do(t *testing.T, c call) *httptest.ResponseRecorder {
	t.Helper()
	if c.method == "" {
		c.method = http.MethodGet
	}
	body := c.body
	if c.form != "" {
		body = c.form
	}
	r := httptest.NewRequestWithContext(context.Background(), c.method, c.target, strings.NewReader(body))
	switch {
	case c.form != "":
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case c.body != "":
		r.Header.Set("Content-Type", "application/json")
	}
	switch {
	case c.auth != "":
		r.Header.Set("Authorization", c.auth)
	case c.key == "-":
	case c.key != "":
		r.Header.Set("Authorization", "Bearer "+c.key)
	default:
		r.Header.Set("Authorization", "Bearer "+f.key)
	}
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	return rec
}

// expect makes c and fails unless the status is want; it returns the body.
func (f *fixture) expect(t *testing.T, c call, want int) []byte {
	t.Helper()
	rec := f.do(t, c)
	if rec.Code != want {
		t.Fatalf("%s %s = %d %s, want %d", c.method, c.target, rec.Code, rec.Body, want)
	}
	return rec.Body.Bytes()
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decoding %q: %v", b, err)
	}
	return v
}

// auditRow is one audit_log row.
type auditRow struct {
	Actor       *uuid.UUID
	Action      string
	SubjectType string
	SubjectID   string
	Data        map[string]any
}

// audit returns the audit rows with action, oldest first.
func audit(t *testing.T, st *store.Store, action string) []auditRow {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(),
		`SELECT actor_user_id, action, subject_type, subject_id, data FROM audit_log WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Actor, &r.Action, &r.SubjectType, &r.SubjectID, &r.Data); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
