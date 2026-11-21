package credited_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/credited"
	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

type changed struct {
	user     uuid.UUID
	from, to time.Time
}

type recorder struct {
	mu    sync.Mutex
	calls []changed
}

func (r *recorder) MinutesChanged(userID uuid.UUID, from, to time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, changed{userID, from, to})
}

type env struct {
	t     *testing.T
	st    *store.Store
	mux   *http.ServeMux
	clock *clock
	pub   *recorder
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := storetest.New(t)
	clk := &clock{t: time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)}
	pub := &recorder{}
	mux := http.NewServeMux()
	credited.NewHandler(credited.NewService(st, pub, credited.WithClock(clk.Now)), httpapitest.StubSession).Register(mux)
	return &env{t: t, st: st, mux: mux, clock: clk, pub: pub}
}

func at(h, m int) string { return fmt.Sprintf("2026-11-20T%02d:%02d:00Z", h, m) }

type item struct {
	Minute     string   `json:"minute"`
	Project    string   `json:"project"`
	Label      string   `json:"label"`
	Credited   int      `json:"credited"`
	Cuts       []string `json:"cuts,omitempty"`
	Flags      []string `json:"flags,omitempty"`
	ComputedAt string   `json:"computed_at"`
	Snapshot   *int64   `json:"snapshot"`
}

func you(minute, project string, credited int) item {
	return item{Minute: minute, Project: project, Label: "you", Credited: credited}
}

// computedAt returns it with the key (computed_at, snapshot).
func (it item) computedAt(computedAt string, snapshot int64) item {
	it.ComputedAt, it.Snapshot = computedAt, &snapshot
	return it
}

// withDefaultKey gives it the key of a run that ended a minute after its
// minute, at snapshot 1, unless it has a key already.
func (it item) withDefaultKey(t *testing.T) item {
	t.Helper()
	if it.ComputedAt != "" || it.Snapshot != nil {
		return it
	}
	m, err := time.Parse(time.RFC3339, it.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return it.computedAt(m.Add(time.Minute).Format(time.RFC3339), 1)
}

type response struct {
	Accepted int `json:"accepted"`
	Rejected []struct {
		Index  int             `json:"index"`
		Error  string          `json:"error"`
		Stored json.RawMessage `json:"stored"`
	} `json:"rejected"`
	ServerTime string `json:"server_time"`
	Err        string `json:"error"`
}

// upload sends items as one batch, giving each item without a key the
// default key (withDefaultKey).
func (e *env) upload(user, device uuid.UUID, items ...item) (int, response) {
	e.t.Helper()
	keyed := make([]item, len(items))
	for i, it := range items {
		keyed[i] = it.withDefaultKey(e.t)
	}
	body, err := json.Marshal(map[string]any{"rules_version": "v1", "minutes": keyed})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.post(user, "/api/v1/devices/"+device.String()+"/minutes", string(body))
}

func (e *env) post(user uuid.UUID, target, body string) (int, response) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httpapitest.Request(e.t, http.MethodPost, target, user, body))
	var res response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		e.t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return rec.Code, res
}

func (e *env) rows(device uuid.UUID) []string {
	e.t.Helper()
	rows, err := e.st.Pool.Query(context.Background(),
		`SELECT minute, project, label, credited FROM credited_minutes WHERE device_id = $1 ORDER BY minute, project`, device)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m time.Time
		var p, l string
		var c int
		if err := rows.Scan(&m, &p, &l, &c); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %s %d", m.UTC().Format("15:04"), p, l, c))
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func rejectedIndexes(r response) []int {
	out := []int{}
	for _, x := range r.Rejected {
		out = append(out, x.Index)
	}
	return out
}

func TestUploadStoresMinutes(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")

	code, res := e.upload(u, d,
		you(at(11, 58), "logbook", 60),
		item{Minute: at(11, 59), Project: "logbook", Label: "agent", Cuts: []string{"agent_alone"}},
		you(at(9, 0), "site", 30), // offline catch-up: first write of an old minute
	)
	if code != http.StatusOK || res.Accepted != 3 || len(res.Rejected) != 0 {
		t.Fatalf("status %d, %+v", code, res)
	}
	want := "[09:00 site you 30 11:58 logbook you 60 11:59 logbook agent 0]"
	if got := fmt.Sprint(e.rows(d)); got != want {
		t.Fatalf("rows = %s, want %s", got, want)
	}
	if len(e.pub.calls) != 1 || e.pub.calls[0].user != u {
		t.Fatalf("publisher calls = %+v", e.pub.calls)
	}
	// An identical retry is accepted and publishes nothing.
	code, res = e.upload(u, d, you(at(11, 58), "logbook", 60))
	if code != http.StatusOK || res.Accepted != 1 {
		t.Fatalf("retry: %d %+v", code, res)
	}
	if len(e.pub.calls) != 1 {
		t.Fatalf("retry published: %+v", e.pub.calls)
	}
	if n := countRows(t, e.st, `SELECT count(*) FROM devices WHERE id = $1 AND last_seen_at IS NOT NULL`, d); n != 1 {
		t.Fatal("last_seen_at not set")
	}
}

func TestUploadRewriteWindow(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	if code, res := e.upload(u, d, you(at(11, 40), "logbook", 30), you(at(11, 50), "logbook", 30)); code != 200 || res.Accepted != 2 {
		t.Fatalf("seed upload: %d %+v", code, res)
	}
	// Ten minutes later, 11:40 is 30 minutes old (final) and 11:50 is 20.
	e.clock.Advance(10 * time.Minute)
	code, res := e.upload(u, d, you(at(11, 40), "logbook", 45), you(at(11, 50), "logbook", 45))
	if code != http.StatusOK || res.Accepted != 1 || fmt.Sprint(rejectedIndexes(res)) != "[0]" {
		t.Fatalf("status %d, %+v", code, res)
	}
	if !strings.Contains(res.Rejected[0].Error, "final") {
		t.Errorf("error = %q", res.Rejected[0].Error)
	}
	if got := fmt.Sprint(e.rows(d)); got != "[11:40 logbook you 30 11:50 logbook you 45]" {
		t.Fatalf("rows = %s", got)
	}
}

func TestUploadRejectsFutureMinutes(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	code, res := e.upload(u, d, you(at(12, 2), "logbook", 10), you(at(12, 3), "logbook", 10))
	if code != http.StatusOK || res.Accepted != 1 || fmt.Sprint(rejectedIndexes(res)) != "[1]" {
		t.Fatalf("status %d, %+v", code, res)
	}
}

func TestUploadCapsMinuteAcrossProjects(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	other := seedtest.Device(t, e.st, u, "desktop")
	if code, _ := e.upload(u, d, you(at(11, 0), "a", 40)); code != 200 {
		t.Fatal("seed upload failed")
	}
	// Stored 40 + 30 > 60: the second batch's item is rejected.
	code, res := e.upload(u, d, you(at(11, 0), "b", 30), you(at(11, 0), "c", 20))
	if code != http.StatusOK || fmt.Sprint(rejectedIndexes(res)) != "[0]" || res.Accepted != 1 {
		t.Fatalf("status %d, %+v", code, res)
	}
	// The cap is per device: another device may credit the same minute.
	if code, res := e.upload(u, other, you(at(11, 0), "a", 60)); code != 200 || res.Accepted != 1 {
		t.Fatalf("other device: %d %+v", code, res)
	}
}

// Two batches that each fit but together exceed 60 s must not both land.
func TestUploadConcurrentBatchesKeepTheCap(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	const workers = 8
	var wg sync.WaitGroup
	accepted := make([]int, workers)
	for i := range workers {
		wg.Go(func() {
			// Each worker credits 40 s to its own project in the same minutes.
			items := make([]item, 0, 20)
			for m := range 20 {
				items = append(items, you(at(11, 30+m), fmt.Sprintf("p%d", i), 40))
			}
			code, res := e.upload(u, d, items...)
			if code != http.StatusOK {
				t.Errorf("worker %d: status %d %+v", i, code, res)
			}
			accepted[i] = res.Accepted
		})
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	var over int
	if err := e.st.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM (
			SELECT minute FROM credited_minutes WHERE device_id = $1 GROUP BY minute HAVING sum(credited) > 60
		) x`, d).Scan(&over); err != nil {
		t.Fatal(err)
	}
	if over != 0 {
		t.Fatalf("%d minutes exceed 60 s", over)
	}
	total := 0
	for _, n := range accepted {
		total += n
	}
	if total != 20 { // exactly one 40 s project fits in each of the 20 minutes
		t.Fatalf("accepted %d items in total, want 20", total)
	}
}

func TestUploadDeviceChecks(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	bob := seedtest.User(t, e.st, "bob", "UTC", true)
	aliceDevice := seedtest.Device(t, e.st, alice, "laptop")
	revoked := seedtest.Device(t, e.st, alice, "old")
	if _, err := e.st.Pool.Exec(context.Background(), `UPDATE devices SET revoked_at = now() WHERE id = $1`, revoked); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		user   uuid.UUID
		target string
		want   int
	}{
		{"other user's device", bob, "/api/v1/devices/" + aliceDevice.String() + "/minutes", http.StatusNotFound},
		{"unknown device", alice, "/api/v1/devices/" + uuid.NewString() + "/minutes", http.StatusNotFound},
		{"malformed id", alice, "/api/v1/devices/nope/minutes", http.StatusNotFound},
		{"revoked device", alice, "/api/v1/devices/" + revoked.String() + "/minutes", http.StatusForbidden},
		{"no auth", uuid.Nil, "/api/v1/devices/" + aliceDevice.String() + "/minutes", http.StatusUnauthorized},
	}
	body := `{"rules_version":"v1","minutes":[{"minute":"` + at(11, 0) + `","project":"x","label":"you","credited":10,` +
		`"computed_at":"` + at(11, 1) + `","snapshot":1}]}`
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, res := e.post(tc.user, tc.target, body); code != tc.want {
				t.Fatalf("status = %d (%+v), want %d", code, res, tc.want)
			}
		})
	}
	if n := countRows(t, e.st, `SELECT count(*) FROM credited_minutes`); n != 0 {
		t.Fatalf("rows written = %d", n)
	}
}

func TestUploadRequestValidation(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	target := "/api/v1/devices/" + d.String() + "/minutes"
	tooMany := make([]item, credited.MaxBatch+1)
	for i := range tooMany {
		tooMany[i] = you(at(10, 0), "x", 0)
	}
	tooManyBody, _ := json.Marshal(map[string]any{"rules_version": "v1", "minutes": tooMany})
	tests := []struct {
		name, body string
		want       int
	}{
		{"unknown rules version", `{"rules_version":"v9","minutes":[]}`, http.StatusBadRequest},
		{"missing rules version", `{"minutes":[]}`, http.StatusBadRequest},
		{"too many minutes", string(tooManyBody), http.StatusBadRequest},
		{"not json", `{`, http.StatusBadRequest},
		{"wrong type", `{"rules_version":"v1","minutes":[{"minute":1}]}`, http.StatusBadRequest},
		{"empty batch", `{"rules_version":"v1","minutes":[]}`, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, res := e.post(u, target, tc.body); code != tc.want {
				t.Fatalf("status = %d (%+v), want %d", code, res, tc.want)
			}
		})
	}
}

// A minute uploaded in real time is closed once final: a later upload can't
// add 'you' time to it under another project.
func TestUploadFinalMinuteTakesNoNewProject(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	if code, res := e.upload(u, d, item{Minute: at(11, 40), Project: "a", Label: "idle", Cuts: []string{"idle"}}); code != 200 || res.Accepted != 1 {
		t.Fatalf("seed upload: %d %+v", code, res)
	}
	e.clock.Advance(5 * time.Hour)
	code, res := e.upload(u, d, you(at(11, 40), "b", 60))
	if code != http.StatusOK || res.Accepted != 0 || len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Error, "final") {
		t.Fatalf("status %d, %+v", code, res)
	}
	if got := fmt.Sprint(e.rows(d)); got != "[11:40 a idle 0]" {
		t.Fatalf("rows = %s", got)
	}
}

// A device can't backfill days before its registration or days it has
// already sealed with an evidence record.
func TestUploadBackfillBounds(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	// Registered yesterday afternoon.
	d := seedtest.DeviceAt(t, e.st, u, "laptop", time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC))
	if _, err := e.st.Pool.Exec(context.Background(), `
		INSERT INTO evidence_days (device_id, day, prev_chain_hash, content_hash, chain_hash, signature, rules_version)
		VALUES ($1, '2026-11-19', '', repeat('a', 64), repeat('b', 64), decode(repeat('00', 64), 'hex'), 'v1')`, d); err != nil {
		t.Fatal(err)
	}
	code, res := e.upload(u, d,
		you("2026-11-18T23:59:00Z", "logbook", 60), // before the registration day
		you("2026-11-19T10:00:00Z", "logbook", 60), // in the sealed day
		you(at(9, 0), "logbook", 60),               // today: fine
	)
	if code != http.StatusOK || res.Accepted != 1 || fmt.Sprint(rejectedIndexes(res)) != "[0 1]" {
		t.Fatalf("status %d, %+v", code, res)
	}
	if !strings.Contains(res.Rejected[0].Error, "registered") || !strings.Contains(res.Rejected[1].Error, "sealed") {
		t.Fatalf("errors = %+v", res.Rejected)
	}
}

func TestUploadCapsProjectsPerMinute(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	items := make([]item, 0, credited.MaxProjectsPerMinute+2)
	for i := range credited.MaxProjectsPerMinute + 2 {
		items = append(items, item{Minute: at(11, 50), Project: fmt.Sprintf("p%d", i), Label: "idle", Cuts: []string{"idle"}})
	}
	code, res := e.upload(u, d, items...)
	if code != http.StatusOK || res.Accepted != credited.MaxProjectsPerMinute || fmt.Sprint(rejectedIndexes(res)) != "[8 9]" {
		t.Fatalf("status %d, %+v", code, res)
	}
	if n := countRows(t, e.st, `SELECT count(*) FROM credited_minutes WHERE device_id = $1`, d); n != credited.MaxProjectsPerMinute {
		t.Fatalf("stored %d rows", n)
	}
}

// Project names are normalised like heartbeat projects.
func TestUploadNormalizesProjects(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	code, res := e.upload(u, d, you(at(11, 50), " logbook ", 30), you(at(11, 51), "<<LAST_PROJECT>>", 30))
	if code != http.StatusOK || res.Accepted != 2 {
		t.Fatalf("status %d, %+v", code, res)
	}
	if got := fmt.Sprint(e.rows(d)); got != "[11:50 logbook you 30 11:51  you 30]" {
		t.Fatalf("rows = %q", got)
	}
}

// A removal (an idle item with no credit, cuts or flags) deletes its row
// from an open minute and frees the project slot for a replacement in the
// same batch, whatever the order of the batch.
func TestUploadReplacesProjectInOpenMinute(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	var items []item
	for _, p := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		items = append(items, you(at(11, 55), p, 7))
	}
	items[0].Credited = 11 // 60 s in all
	if code, res := e.upload(u, d, items...); code != 200 || res.Accepted != 8 {
		t.Fatalf("seed upload: %d %+v", code, res)
	}
	// i replaces h; the replacement is listed before the removal.
	code, res := e.upload(u, d, you(at(11, 55), "i", 7), removal(at(11, 55), "h"))
	if code != http.StatusOK || res.Accepted != 2 || len(res.Rejected) != 0 {
		t.Fatalf("status %d, %+v", code, res)
	}
	want := "[11:55 a you 11 11:55 b you 7 11:55 c you 7 11:55 d you 7 11:55 e you 7 11:55 f you 7 11:55 g you 7 11:55 i you 7]"
	if got := fmt.Sprint(e.rows(d)); got != want {
		t.Fatalf("rows = %s", got)
	}
	// A retried removal is accepted and changes nothing.
	if code, res := e.upload(u, d, removal(at(11, 55), "h")); code != 200 || res.Accepted != 1 {
		t.Fatalf("retry: %d %+v", code, res)
	}
	if got := fmt.Sprint(e.rows(d)); got != want {
		t.Fatalf("rows after retry = %s", got)
	}
}

// A client can't use removals or zero-credit rows to store more than 8
// rows or 60 seconds for a device-minute, nor to change a final minute.
func TestUploadRemovalsKeepBounds(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	m := at(11, 50)
	// Zero-credit rows that aren't removals still take slots.
	var items []item
	for i := range credited.MaxProjectsPerMinute + 1 {
		items = append(items, item{Minute: m, Project: fmt.Sprintf("z%d", i), Label: "agent", Cuts: []string{"agent_alone"}})
	}
	if code, res := e.upload(u, d, items...); code != 200 || fmt.Sprint(rejectedIndexes(res)) != "[8]" {
		t.Fatalf("zero rows: %d %+v", code, res)
	}
	// Removals of rows that aren't stored free nothing.
	code, res := e.upload(u, d, removal(m, "x1"), removal(m, "x2"), you(m, "new", 10))
	if code != 200 || fmt.Sprint(rejectedIndexes(res)) != "[2]" || !strings.Contains(res.Rejected[0].Error, "8 projects") {
		t.Fatalf("removals of nothing: %d %+v", code, res)
	}
	// Churn: remove one row and add two, many times over. Never more than
	// 8 rows or 60 seconds.
	for i := range 20 {
		code, res = e.upload(u, d, removal(m, fmt.Sprintf("z%d", i%8)),
			you(m, fmt.Sprintf("n%d", i), 10), you(m, fmt.Sprintf("o%d", i), 10))
		if code != 200 {
			t.Fatalf("churn %d: %d %+v", i, code, res)
		}
		if n := countRows(t, e.st, `SELECT count(*) FROM credited_minutes WHERE device_id = $1 AND minute = $2`, d, m); n > credited.MaxProjectsPerMinute {
			t.Fatalf("churn %d: %d rows stored", i, n)
		}
		if n := countRows(t, e.st, `SELECT coalesce(sum(credited), 0)::int FROM credited_minutes WHERE device_id = $1 AND minute = $2`, d, m); n > 60 {
			t.Fatalf("churn %d: %d s stored", i, n)
		}
	}
	// A final minute can't lose a row.
	before := e.rows(d)
	e.clock.Advance(30 * time.Minute)
	code, res = e.upload(u, d, removal(m, strings.Fields(before[0])[1]))
	if code != 200 || len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Error, "final") {
		t.Fatalf("removal in a final minute: %d %+v", code, res)
	}
	if got := e.rows(d); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("final minute changed: %s, was %s", got, before)
	}
}

// An empty batch is accepted and changes nothing: the agent sends one to
// read the server's clock from the body before sealing a day.
func TestUploadEmptyBatch(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	code, res := e.upload(u, d)
	if code != http.StatusOK || res.Accepted != 0 || len(res.Rejected) != 0 {
		t.Fatalf("status %d, %+v", code, res)
	}
	if res.ServerTime != "2026-11-20T12:00:30Z" {
		t.Fatalf("server_time = %q, want the server's clock as RFC 3339 UTC", res.ServerTime)
	}
	if len(e.rows(d)) != 0 || len(e.pub.calls) != 0 {
		t.Fatal("an empty batch changed something")
	}
}

// keys returns the stored "hh:mm project computed_at/snapshot" of device's
// rows, with "null" for a missing key.
func (e *env) keys(device uuid.UUID) []string {
	e.t.Helper()
	rows, err := e.st.Pool.Query(context.Background(), `
		SELECT minute, project, computed_at, snapshot FROM credited_minutes
		WHERE device_id = $1 ORDER BY minute, project`, device)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m time.Time
		var p string
		var computedAt *time.Time
		var snapshot *int64
		if err := rows.Scan(&m, &p, &computedAt, &snapshot); err != nil {
			e.t.Fatal(err)
		}
		key := "null"
		if computedAt != nil && snapshot != nil {
			key = fmt.Sprintf("%s/%d", computedAt.UTC().Format("15:04"), *snapshot)
		}
		out = append(out, fmt.Sprintf("%s %s %s", m.UTC().Format("15:04"), p, key))
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// Each row stores the key it was uploaded with, and a new key rewrites the
// row while its minute is open (ADR 0017).
func TestUploadStoresKeys(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	code, res := e.upload(u, d,
		you(at(11, 50), "a", 30).computedAt(at(11, 51), 40),
		you(at(11, 50), "b", 30).computedAt(at(11, 51), 40),
		you(at(11, 0), "a", 60).computedAt(at(11, 59), 0),
	)
	if code != http.StatusOK || res.Accepted != 3 {
		t.Fatalf("status %d, %+v", code, res)
	}
	want := "[11:00 a 11:59/0 11:50 a 11:51/40 11:50 b 11:51/40]"
	if got := fmt.Sprint(e.keys(d)); got != want {
		t.Fatalf("keys = %s, want %s", got, want)
	}
	// Same values at a later key: a rewrite of the open minute.
	code, res = e.upload(u, d,
		you(at(11, 50), "a", 30).computedAt(at(11, 52), 41),
		you(at(11, 50), "b", 30).computedAt(at(11, 52), 41),
	)
	if code != http.StatusOK || res.Accepted != 2 {
		t.Fatalf("rewrite: %d %+v", code, res)
	}
	want = "[11:00 a 11:59/0 11:50 a 11:52/41 11:50 b 11:52/41]"
	if got := fmt.Sprint(e.keys(d)); got != want {
		t.Fatalf("keys after rewrite = %s, want %s", got, want)
	}
	if len(e.pub.calls) != 2 {
		t.Fatalf("a key change should publish: %+v", e.pub.calls)
	}
}

// Once a minute is final, a changed key is refused like a changed value,
// and the rejection carries the rows the server holds for the minute. The
// exact stored row, key included, is still accepted.
func TestUploadKeyChangeOnceFinal(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	a := you(at(11, 50), "a", 40).computedAt(at(12, 0), 41)
	b := item{Minute: at(11, 50), Project: "b", Label: "idle", Cuts: []string{"idle"}}.computedAt(at(12, 0), 41)
	if code, res := e.upload(u, d, b, a); code != 200 || res.Accepted != 2 {
		t.Fatalf("seed upload: %d %+v", code, res)
	}
	e.clock.Advance(30 * time.Minute)

	// Same value, later key; and a removal of b carrying its key.
	code, res := e.upload(u, d, you(at(11, 50), "a", 40).computedAt(at(12, 1), 42),
		removal(at(11, 50), "b").computedAt(at(12, 1), 42))
	if code != http.StatusOK || res.Accepted != 0 || fmt.Sprint(rejectedIndexes(res)) != "[0 1]" {
		t.Fatalf("status %d, %+v", code, res)
	}
	wantStored := `[{"project":"a","label":"you","credited":40,"cuts":[],"flags":[],"computed_at":"2026-11-20T12:00:00Z","snapshot":41},` +
		`{"project":"b","label":"idle","credited":0,"cuts":["idle"],"flags":[],"computed_at":"2026-11-20T12:00:00Z","snapshot":41}]`
	for _, r := range res.Rejected {
		if !strings.Contains(r.Error, "final") || string(r.Stored) != wantStored {
			t.Errorf("rejected[%d] = %q stored %s, want %s", r.Index, r.Error, r.Stored, wantStored)
		}
	}
	if got := fmt.Sprint(e.keys(d)); got != "[11:50 a 12:00/41 11:50 b 12:00/41]" {
		t.Fatalf("keys = %s", got)
	}

	// The exact stored rows are a repeat and accepted.
	if code, res := e.upload(u, d, a, b); code != 200 || res.Accepted != 2 || len(res.Rejected) != 0 {
		t.Fatalf("repeat: %d %+v", code, res)
	}
}

// A row stored before ADR 0017 has no key; once final, it is returned with
// null computed_at and snapshot.
func TestUploadFinalLegacyRow(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	seedtest.CreditedSpan(t, e.st, u, d, time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC), 1,
		seedtest.Credited{Project: "a", Label: "you", Seconds: 60})
	code, res := e.upload(u, d, you(at(11, 0), "a", 60))
	if code != http.StatusOK || len(res.Rejected) != 1 {
		t.Fatalf("status %d, %+v", code, res)
	}
	want := `[{"project":"a","label":"you","credited":60,"cuts":[],"flags":[],"computed_at":null,"snapshot":null}]`
	if string(res.Rejected[0].Stored) != want {
		t.Fatalf("stored = %s, want %s", res.Rejected[0].Stored, want)
	}
}

// Malformed keys are rejected per item; a snapshot that isn't an integer
// fails the whole request like any other wrongly typed field.
func TestUploadKeyValidation(t *testing.T) {
	e := newEnv(t)
	u := seedtest.User(t, e.st, "alice", "UTC", true)
	d := seedtest.Device(t, e.st, u, "laptop")
	target := "/api/v1/devices/" + d.String() + "/minutes"
	minute := func(extra string) string {
		return `{"minute":"` + at(11, 50) + `","project":"a","label":"you","credited":10` + extra + `}`
	}
	body := `{"rules_version":"v1","minutes":[` + strings.Join([]string{
		minute(`,"snapshot":1`),
		minute(`,"computed_at":"` + at(11, 51) + `"`),
		minute(`,"computed_at":"` + at(11, 50) + `","snapshot":1`),
		minute(`,"computed_at":"` + at(12, 51) + `","snapshot":1`),
		minute(`,"computed_at":"2026-11-20T11:51:30Z","snapshot":1`),
		minute(`,"computed_at":"` + at(11, 51) + `","snapshot":-1`),
		minute(`,"computed_at":"` + at(11, 51) + `","snapshot":null`),
	}, ",") + `]}`
	code, res := e.post(u, target, body)
	if code != http.StatusOK || res.Accepted != 0 || len(res.Rejected) != 7 {
		t.Fatalf("status %d, %+v", code, res)
	}
	wantErr := []string{"computed_at", "snapshot", "60 minutes before", "60 minutes before", "computed_at", "snapshot", "snapshot"}
	for i, r := range res.Rejected {
		if !strings.Contains(r.Error, wantErr[i]) || r.Stored != nil {
			t.Errorf("rejected[%d] = %+v, want an error about %s", i, r, wantErr[i])
		}
	}
	for _, bad := range []string{`1.5`, `"1"`, `1e30`} {
		body := `{"rules_version":"v1","minutes":[` + minute(`,"computed_at":"`+at(11, 51)+`","snapshot":`+bad) + `]}`
		if code, res := e.post(u, target, body); code != http.StatusBadRequest {
			t.Errorf("snapshot %s: status %d %+v, want 400", bad, code, res)
		}
	}
	if len(e.rows(d)) != 0 {
		t.Fatal("invalid items were stored")
	}
}

// Another user can neither write a device's minutes nor read its stored
// rows: their upload is a 404, and a "final" rejection on their own device
// returns only that device's rows.
func TestUploadKeysAreScopedToTheDevice(t *testing.T) {
	e := newEnv(t)
	alice := seedtest.User(t, e.st, "alice", "UTC", true)
	bob := seedtest.User(t, e.st, "bob", "UTC", true)
	aliceDevice := seedtest.Device(t, e.st, alice, "laptop")
	bobDevice := seedtest.Device(t, e.st, bob, "laptop")
	if code, res := e.upload(alice, aliceDevice, you(at(11, 50), "secret", 40)); code != 200 || res.Accepted != 1 {
		t.Fatalf("alice: %d %+v", code, res)
	}
	if code, res := e.upload(bob, bobDevice, you(at(11, 50), "mine", 20)); code != 200 || res.Accepted != 1 {
		t.Fatalf("bob: %d %+v", code, res)
	}
	e.clock.Advance(30 * time.Minute)

	code, res := e.upload(bob, aliceDevice, you(at(11, 50), "secret", 40).computedAt(at(11, 52), 9))
	if code != http.StatusNotFound || len(res.Rejected) != 0 {
		t.Fatalf("bob on alice's device: %d %+v", code, res)
	}
	code, res = e.upload(bob, bobDevice, you(at(11, 50), "secret", 40).computedAt(at(11, 52), 9))
	if code != http.StatusOK || len(res.Rejected) != 1 {
		t.Fatalf("bob on his device: %d %+v", code, res)
	}
	if s := string(res.Rejected[0].Stored); strings.Contains(s, "secret") || !strings.Contains(s, `"mine"`) {
		t.Fatalf("bob's stored rows = %s", s)
	}
	if got := fmt.Sprint(e.keys(aliceDevice)); got != "[11:50 secret 11:51/1]" {
		t.Fatalf("alice's rows = %s", got)
	}
}

func removal(minute, project string) item {
	return item{Minute: minute, Project: project, Label: "idle"}
}

func countRows(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
