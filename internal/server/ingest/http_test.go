package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/ingest"
	"github.com/apneduniya/logbook/internal/server/store"
)

const testUserHeader = "X-Test-User"

// stubAPIKey stands in for auth.RequireAPIKey: it trusts a test header and
// answers 401 without it, like the real middleware does without a key.
func stubAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.Header.Get(testUserHeader))
		if err != nil {
			httpapi.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r.WithContext(httpapi.WithUser(r.Context(), httpapi.CurrentUser{ID: id, Via: "api_key"})))
	})
}

type server struct {
	st  *store.Store
	mux *http.ServeMux
}

func newServer(t *testing.T, opts ...ingest.HandlerOption) *server {
	t.Helper()
	svc, st, _ := newService(t)
	mux := http.NewServeMux()
	ingest.NewHandler(svc, stubAPIKey, opts...).Register(mux)
	return &server{st: st, mux: mux}
}

// cliUserAgent is what wakatime-cli sends for the VS Code plugin.
const cliUserAgent = "wakatime/v1.102.1 (linux-6.8.0-49-generic-x86_64) go1.23.4 vscode/1.95.3 vscode-wakatime/24.9.1"

func (s *server) do(t *testing.T, method, path string, user uuid.UUID, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	// Headers wakatime-cli sets on every request (pkg/api/option.go, heartbeat.go).
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cliUserAgent)
	req.Header.Set("Authorization", "Basic d2FrYV90ZXN0")
	if user != uuid.Nil {
		req.Header.Set(testUserHeader, user.String())
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// cliBulkRequest is wakatime-cli's pkg/api/testdata/api_heartbeats_request.json.
const cliBulkRequest = `[
	{
		"branch": "heartbeat",
		"category": "coding",
		"cursorpos": 12,
		"dependencies": ["dep1", "dep2"],
		"entity": "/tmp/main.go",
		"is_write": true,
		"language": "Go",
		"lineno": 42,
		"lines": 100,
		"project": "wakatime-cli",
		"type": "file",
		"time": 1585598059,
		"user_agent": "wakatime/13.0.6"
	},
	{
		"category": "debugging",
		"entity": "HIDDEN.py",
		"type": "file",
		"time": 1585598060,
		"user_agent": "wakatime/13.0.7"
	}
]`

// cliResult mirrors how wakatime-cli's ParseHeartbeatResponses reads a bulk
// response: responses[i] = [body, status]; 2xx bodies carry data.id, others
// carry "error" (or "errors").
type cliResult struct {
	Status int
	ID     string
	Error  string
}

func parseBulk(t *testing.T, body []byte) []cliResult {
	t.Helper()
	var resp struct {
		Responses [][]json.RawMessage `json:"responses"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("bulk response %s: %v", body, err)
	}
	out := make([]cliResult, len(resp.Responses))
	for i, r := range resp.Responses {
		if len(r) != 2 {
			t.Fatalf("response %d has %d elements, want 2", i, len(r))
		}
		if err := json.Unmarshal(r[1], &out[i].Status); err != nil {
			t.Fatalf("response %d status: %v", i, err)
		}
		var b struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(r[0], &b); err != nil {
			t.Fatalf("response %d body: %v", i, err)
		}
		out[i].ID, out[i].Error = b.Data.ID, b.Error
	}
	return out
}

func TestBulkAcceptsWakatimeCLIRequest(t *testing.T) {
	for _, prefix := range []string{"/api/v1", "/api/hackatime/v1"} {
		t.Run(prefix, func(t *testing.T) {
			s := newServer(t)
			user := newUser(t, s.st, "UTC")
			rec := s.do(t, http.MethodPost, prefix+"/users/current/heartbeats.bulk", user, cliBulkRequest,
				map[string]string{"X-Machine-Name": "my%20laptop"})
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q", ct)
			}
			results := parseBulk(t, rec.Body.Bytes())
			if len(results) != 2 {
				t.Fatalf("got %d results, want 2", len(results))
			}
			for i, r := range results {
				if r.Status != http.StatusCreated || len(r.ID) != 64 {
					t.Fatalf("result %d = %+v, want 201 with an id", i, r)
				}
			}

			var machine, editor, os, plugin, branch string
			var deps []string
			err := s.st.Pool.QueryRow(context.Background(),
				`SELECT machine, editor, operating_system, plugin, branch, dependencies FROM heartbeats
				 WHERE user_id = $1 AND entity = '/tmp/main.go'`, user).Scan(&machine, &editor, &os, &plugin, &branch, &deps)
			if err != nil {
				t.Fatal(err)
			}
			// The body's user_agent ("wakatime/13.0.6") names no editor, so editor stays empty.
			if machine != "my laptop" || editor != "" || branch != "heartbeat" || len(deps) != 2 {
				t.Fatalf("stored machine=%q editor=%q os=%q plugin=%q branch=%q deps=%v", machine, editor, os, plugin, branch, deps)
			}
		})
	}
}

func TestHeaderUserAgentFillsEditorAndOS(t *testing.T) {
	s := newServer(t)
	user := newUser(t, s.st, "UTC")
	body := fmt.Sprintf(`[{"entity":"/src/a.go","type":"file","time":%f,"project":"p","editor":"body-editor"}]`, unix(base))
	rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", user, body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var editor, os, plugin, ua, category string
	err := s.st.Pool.QueryRow(context.Background(),
		`SELECT editor, operating_system, plugin, user_agent, category FROM heartbeats WHERE user_id = $1`, user).
		Scan(&editor, &os, &plugin, &ua, &category)
	if err != nil {
		t.Fatal(err)
	}
	if editor != "vscode" || os != "linux" || plugin != "vscode-wakatime/24.9.1" || ua != cliUserAgent || category != "coding" {
		t.Fatalf("stored editor=%q os=%q plugin=%q ua=%q category=%q", editor, os, plugin, ua, category)
	}
}

func TestSingleHeartbeat(t *testing.T) {
	valid := fmt.Sprintf(`{"entity":"/src/a.go","type":"file","time":%f,"project":"p"}`, unix(base))
	tests := []struct {
		name, path, body string
		wantStatus       int
		wantError        bool
	}{
		{"wakatime object", "/api/v1/users/current/heartbeats", valid, http.StatusCreated, false},
		{"hackatime object", "/api/hackatime/v1/users/current/heartbeats", valid, http.StatusAccepted, false},
		{"one-element array", "/api/v1/users/current/heartbeats", "[" + valid + "]", http.StatusCreated, false},
		{"two-element array", "/api/v1/users/current/heartbeats", "[" + valid + "," + valid + "]", http.StatusBadRequest, true},
		{"missing entity", "/api/v1/users/current/heartbeats", fmt.Sprintf(`{"time":%f}`, unix(base)), http.StatusBadRequest, true},
		{"time in the future", "/api/v1/users/current/heartbeats", fmt.Sprintf(`{"entity":"a","time":%f}`, unix(now.Add(2*time.Hour))), http.StatusBadRequest, true},
		{"wrong type", "/api/v1/users/current/heartbeats", `{"entity":"a","time":"yesterday"}`, http.StatusBadRequest, true},
		{"not json", "/api/v1/users/current/heartbeats", `{"entity":`, http.StatusBadRequest, true},
		{"scalar", "/api/v1/users/current/heartbeats", `42`, http.StatusBadRequest, true},
		{"empty array", "/api/v1/users/current/heartbeats", `[]`, http.StatusBadRequest, true},
	}
	s := newServer(t)
	user := newUser(t, s.st, "UTC")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := s.do(t, http.MethodPost, tt.path, user, tt.body, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			var body struct {
				Data struct {
					ID     string `json:"id"`
					Entity string `json:"entity"`
				} `json:"data"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if tt.wantError != (body.Error != "") {
				t.Fatalf("error = %q, want error %v", body.Error, tt.wantError)
			}
			if !tt.wantError && (body.Data.ID == "" || body.Data.Entity != "/src/a.go") {
				t.Fatalf("data = %+v", body.Data)
			}
		})
	}
}

func TestBulkPerItemErrors(t *testing.T) {
	s := newServer(t)
	user := newUser(t, s.st, "UTC")
	body := fmt.Sprintf(`[
		{"entity":"/src/a.go","time":%f},
		{"time":%f},
		"not a heartbeat",
		{"entity":"/src/b.go","time":"soon"},
		{"entity":"/src/c.go","time":%f,"lineno":-5},
		{"entity":"/src/a.go","time":%f}
	]`, unix(base), unix(base), unix(base.Add(time.Second)), unix(base))
	rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", user, body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	results := parseBulk(t, rec.Body.Bytes())
	wantStatus := []int{201, 400, 400, 400, 400, 201}
	if len(results) != len(wantStatus) {
		t.Fatalf("got %d results, want %d", len(results), len(wantStatus))
	}
	for i, r := range results {
		if r.Status != wantStatus[i] {
			t.Fatalf("result %d status = %d, want %d (%+v)", i, r.Status, wantStatus[i], r)
		}
		if r.Status == 400 && r.Error == "" {
			t.Fatalf("result %d has no error message", i)
		}
	}
	if results[0].ID != results[5].ID {
		t.Fatal("a repeated heartbeat should report the same id")
	}
	if n := countRows(t, s.st, "SELECT count(*) FROM heartbeats WHERE user_id = $1", user); n != 1 {
		t.Fatalf("stored %d heartbeats, want 1", n)
	}
}

func TestBulkLimit(t *testing.T) {
	s := newServer(t)
	user := newUser(t, s.st, "UTC")
	build := func(n int) string {
		var b bytes.Buffer
		b.WriteString("[")
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"entity":"/f%d","time":%f}`, i, unix(base.Add(time.Duration(i)*time.Second)))
		}
		b.WriteString("]")
		return b.String()
	}
	if rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", user, build(100), nil); rec.Code != http.StatusCreated {
		t.Fatalf("100 heartbeats: status = %d, body %s", rec.Code, rec.Body)
	}
	rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", user, build(101), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "max 100") {
		t.Fatalf("101 heartbeats: status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestEndpointsRequireAPIKey(t *testing.T) {
	s := newServer(t)
	for _, ep := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/users/current/heartbeats"},
		{http.MethodPost, "/api/v1/users/current/heartbeats.bulk"},
		{http.MethodGet, "/api/v1/users/current/statusbar/today"},
		{http.MethodPost, "/api/hackatime/v1/users/current/heartbeats"},
		{http.MethodPost, "/api/hackatime/v1/users/current/heartbeats.bulk"},
		{http.MethodGet, "/api/hackatime/v1/users/current/statusbar/today"},
	} {
		if rec := s.do(t, ep.method, ep.path, uuid.Nil, `{}`, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without key: status = %d", ep.method, ep.path, rec.Code)
		}
	}
}

func TestRateLimit(t *testing.T) {
	s := newServer(t, ingest.WithRateLimit(0.001, 2))
	alice, bob := newUser(t, s.st, "UTC"), newUser(t, s.st, "UTC")
	body := fmt.Sprintf(`{"entity":"a","time":%f}`, unix(base))
	for i := range 2 {
		if rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats", alice, body, nil); rec.Code != http.StatusCreated {
			t.Fatalf("request %d: status = %d", i, rec.Code)
		}
	}
	rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", alice, "["+body+"]", nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over limit: status = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// Limits are per user.
	if rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats", bob, body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("other user: status = %d", rec.Code)
	}
}

func TestStatusBarRateLimit(t *testing.T) {
	s := newServer(t, ingest.WithRateLimit(0.001, 1), ingest.WithStatusBarRateLimit(0.001, 2))
	alice, bob := newUser(t, s.st, "UTC"), newUser(t, s.st, "UTC")
	// Heartbeats and the status bar use separate buckets: spending the
	// heartbeat bucket leaves status-bar reads available, and vice versa.
	body := fmt.Sprintf(`{"entity":"a","time":%f}`, unix(base))
	if rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats", alice, body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("heartbeat: status = %d", rec.Code)
	}
	for _, prefix := range []string{"/api/v1", "/api/hackatime/v1"} {
		if rec := s.do(t, http.MethodGet, prefix+"/users/current/statusbar/today", alice, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("%s within limit: status = %d", prefix, rec.Code)
		}
	}
	rec := s.do(t, http.MethodGet, "/api/v1/users/current/statusbar/today", alice, "", nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over limit: status = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := s.do(t, http.MethodGet, "/api/v1/users/current/statusbar/today", bob, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("other user: status = %d", rec.Code)
	}
}

// TestBulkAcceptsNegativeLineChanges uses the net line deltas wakatime-cli
// computes for AI and human edits (pkg/ai), which are negative when an edit
// removes lines. A 400 here would make the CLI drop the heartbeat for good.
func TestBulkAcceptsNegativeLineChanges(t *testing.T) {
	s := newServer(t)
	user := newUser(t, s.st, "UTC")
	body := fmt.Sprintf(`[{"entity":"/src/a.go","type":"file","category":"ai coding","time":%f,
		"ai_line_changes":-5,"human_line_changes":-2,"ai_model":"gpt-5"}]`, unix(base))
	rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", user, body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if r := parseBulk(t, rec.Body.Bytes()); len(r) != 1 || r[0].Status != http.StatusCreated {
		t.Fatalf("results = %+v, want one 201", r)
	}
	var ai, human int
	err := s.st.Pool.QueryRow(context.Background(),
		`SELECT ai_line_changes, human_line_changes FROM heartbeats WHERE user_id = $1`, user).Scan(&ai, &human)
	if err != nil {
		t.Fatal(err)
	}
	if ai != -5 || human != -2 {
		t.Fatalf("stored ai_line_changes=%d human_line_changes=%d", ai, human)
	}
}

func TestHeartbeatsAreStoredForTheCallerOnly(t *testing.T) {
	s := newServer(t)
	alice, bob := newUser(t, s.st, "UTC"), newUser(t, s.st, "UTC")
	body := fmt.Sprintf(`[{"entity":"a","time":%f,"project":"secret"},{"entity":"b","time":%f,"project":"secret"}]`,
		unix(base), unix(base.Add(time.Minute)))
	if rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", bob, body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	for table, want := range map[string]int{"heartbeats": 0, "user_minutes": 0, "projects": 0} {
		if n := countRows(t, s.st, "SELECT count(*) FROM "+table+" WHERE user_id = $1", alice); n != want {
			t.Fatalf("alice has %d %s rows after bob's upload", n, table)
		}
	}
	if n := countRows(t, s.st, "SELECT count(*) FROM user_minutes WHERE user_id = $1", bob); n == 0 {
		t.Fatal("bob's upload produced no rollups")
	}
}

type statusBar struct {
	Data struct {
		GrandTotal struct {
			Decimal      string  `json:"decimal"`
			Digital      string  `json:"digital"`
			Hours        int     `json:"hours"`
			Minutes      int     `json:"minutes"`
			Text         string  `json:"text"`
			TotalSeconds float64 `json:"total_seconds"`
		} `json:"grand_total"`
		Projects []struct {
			Name         string  `json:"name"`
			TotalSeconds float64 `json:"total_seconds"`
			Percent      float64 `json:"percent"`
			Text         string  `json:"text"`
		} `json:"projects"`
		Categories []struct {
			Name string `json:"name"`
		} `json:"categories"`
		Range struct {
			Date     string `json:"date"`
			Start    string `json:"start"`
			End      string `json:"end"`
			Text     string `json:"text"`
			Timezone string `json:"timezone"`
		} `json:"range"`
	} `json:"data"`
}

func TestStatusBarToday(t *testing.T) {
	s := newServer(t)
	// now is 2026-11-19 12:00 UTC = 17:30 in Kolkata, whose day began at 18:30 UTC yesterday.
	alice := newUser(t, s.st, "Asia/Kolkata")
	bob := newUser(t, s.st, "Asia/Kolkata")

	post := func(user uuid.UUID, start time.Time, n int, project string) {
		t.Helper()
		var parts []string
		for i := range n {
			parts = append(parts, fmt.Sprintf(`{"entity":"/x/%d","time":%f,"project":%q,"language":"Go"}`,
				i, unix(start.Add(time.Duration(i)*30*time.Second)), project))
		}
		rec := s.do(t, http.MethodPost, "/api/v1/users/current/heartbeats.bulk", user, "["+strings.Join(parts, ",")+"]", nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("posting: status %d", rec.Code)
		}
	}
	yesterdayLocal := time.Date(2026, 11, 18, 17, 0, 0, 0, time.UTC) // 22:30 IST on the 18th
	post(alice, yesterdayLocal, 5, "old")
	// Raw time follows Hackatime: every gap counts up to 2 minutes, credited to
	// the later heartbeat. So the first "logbook" heartbeat gets 2 minutes for
	// the overnight break, and the "" burst gets 2 minutes for its gap.
	post(alice, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC), 61, "logbook")   // 30 minutes + 2
	post(alice, time.Date(2026, 11, 19, 9, 30, 30, 0, time.UTC), 60, "logbook") // 30 more, joined by a 30s gap
	post(alice, time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC), 3, "")          // 1 minute + 2, Unknown
	post(bob, time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), 61, "bobs")

	for _, prefix := range []string{"/api/v1", "/api/hackatime/v1"} {
		rec := s.do(t, http.MethodGet, prefix+"/users/current/statusbar/today", alice, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body %s", prefix, rec.Code, rec.Body)
		}
		var sb statusBar
		if err := json.Unmarshal(rec.Body.Bytes(), &sb); err != nil {
			t.Fatal(err)
		}
		gt := sb.Data.GrandTotal
		if gt.TotalSeconds != 3900 || gt.Text != "1 hr 5 mins" || gt.Digital != "1:05" || gt.Hours != 1 || gt.Minutes != 5 || gt.Decimal != "1.08" {
			t.Fatalf("grand_total = %+v", gt)
		}
		if len(sb.Data.Projects) != 2 || sb.Data.Projects[0].Name != "logbook" || sb.Data.Projects[1].Name != "Unknown" ||
			sb.Data.Projects[0].Text != "1 hr 2 mins" || sb.Data.Projects[1].Percent != 4.62 {
			t.Fatalf("projects = %+v", sb.Data.Projects)
		}
		if len(sb.Data.Categories) != 1 || sb.Data.Categories[0].Name != "Coding" {
			t.Fatalf("categories = %+v", sb.Data.Categories)
		}
		r := sb.Data.Range
		if r.Date != "2026-11-19" || r.Timezone != "Asia/Kolkata" || r.Start != "2026-11-18T18:30:00Z" ||
			r.End != "2026-11-19T18:29:59Z" || r.Text != "Thu Nov 19th 2026" {
			t.Fatalf("range = %+v", r)
		}
	}

	// The response must be parseable as wakatime-cli's summary.Summary, which
	// needs every list to be an array, never null.
	rec := s.do(t, http.MethodGet, "/api/v1/users/current/statusbar/today", newUser(t, s.st, "UTC"), "", nil)
	var raw struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"categories", "dependencies", "editors", "languages", "machines", "operating_systems", "projects"} {
		if _, ok := raw.Data[k].([]any); !ok {
			t.Fatalf("data.%s = %v, want an array", k, raw.Data[k])
		}
	}
	if gt, _ := raw.Data["grand_total"].(map[string]any); gt["text"] != "0 secs" || gt["total_seconds"] != 0.0 {
		t.Fatalf("empty grand_total = %v", gt)
	}
}

func TestStatusBarUnknownUser(t *testing.T) {
	s := newServer(t)
	rec := s.do(t, http.MethodGet, "/api/v1/users/current/statusbar/today", uuid.New(), "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}
