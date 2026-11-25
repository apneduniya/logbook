package admin_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/hackatime/*.golden")

// contractCase is one Hackatime-compatible request, pinned with its full
// response. source names the Hackatime code (43bde10) the shape follows.
type contractCase struct {
	name   string
	source string
	c      call
}

func contractCases(f *fixture) []contractCase {
	id := strconv.FormatInt(f.orpheusID, 10)
	pat := strconv.FormatInt(f.patID, 10)
	const uu = "user_utilities.rb"
	return []contractCase{
		{"user_info", uu + " user_info", call{target: "/api/admin/v1/user/info?id=" + id}},
		{"user_info_unlinked", uu + " user_info, a user without Hackatime or activity",
			call{target: "/api/admin/v1/user/info?user_id=" + pat}},
		{"user_info_who", uu + " find_user_by_id, no id", call{target: "/api/admin/v1/user/info"}},
		{"user_info_not_found", uu + " find_user_by_id, unknown id", call{target: "/api/admin/v1/user/info?id=7"}},
		{"user_info_batch", uu + " user_info_batch",
			call{target: "/api/admin/v1/user/info_batch?ids=" + id + ",%20" + pat + "," + id + ",abc,999"}},
		{"user_info_batch_missing", uu + " user_info_batch, no ids", call{target: "/api/admin/v1/user/info_batch?ids="}},
		{"user_info_batch_empty", uu + " user_info_batch, only separators", call{target: "/api/admin/v1/user/info_batch?ids=,,"}},
		{"user_heartbeats", uu + " user_heartbeats, a page with filters",
			call{target: "/api/admin/v1/user/heartbeats?id=" + id + "&project=logbook&start_date=2026-06-10&end_date=2026-06-10&limit=3&offset=1"}},
		{"user_heartbeats_ai", uu + " user_heartbeats with include_ai, the last page",
			call{target: "/api/admin/v1/user/heartbeats?id=" + id + "&include_ai=1&limit=2&offset=41"}},
		{"user_heartbeats_bad_date", uu + " apply_time_range, an unparsable date",
			call{target: "/api/admin/v1/user/heartbeats?id=" + id + "&start_date=yesterday"}},
		{"user_projects", uu + " user_projects", call{target: "/api/admin/v1/user/projects?id=" + id}},
		{"user_projects_range", uu + " user_projects with start_date (parse_default_time_range)",
			call{target: "/api/admin/v1/user/projects?id=" + id + "&start_date=1781085600"}},
		{"user_trust_logs", uu + " trust_logs", call{target: "/api/admin/v1/user/trust_logs?id=" + id}},
		{"get_user_by_email", uu + " get_user_by_email",
			call{method: http.MethodPost, target: "/api/admin/v1/user/get_user_by_email", body: `{"email": "orpheus@example.com"}`}},
		{"get_user_by_email_form", uu + " get_user_by_email, form body, unknown email",
			call{method: http.MethodPost, target: "/api/admin/v1/user/get_user_by_email", form: "email=nobody%40example.com"}},
		{"get_user_by_email_blank", uu + " get_user_by_email, no email",
			call{method: http.MethodPost, target: "/api/admin/v1/user/get_user_by_email", body: `{}`}},
		{"search_fuzzy", uu + " search_users_fuzzy (UserFuzzySearch.fuzzy_ranked_search)",
			call{method: http.MethodPost, target: "/api/admin/v1/user/search_fuzzy", body: `{"query": " orph "}`}},
		{"search_fuzzy_id", uu + " search_users_fuzzy by id",
			call{method: http.MethodPost, target: "/api/admin/v1/user/search_fuzzy?query=" + id}},
		{"search_fuzzy_blank", uu + " search_users_fuzzy, no query",
			call{method: http.MethodPost, target: "/api/admin/v1/user/search_fuzzy", body: `{"query": ""}`}},
		{"unsupported_stats", "routes.rb user/stats: not provided", call{target: "/api/admin/v1/user/stats?id=" + id}},
		{"unsupported_convict", "routes.rb user/convict: not provided (writes)",
			call{method: http.MethodPost, target: "/api/admin/v1/user/convict", body: `{"id": ` + id + `}`}},
		{"wrong_method", "routes.rb: user/info is GET only", call{method: http.MethodPost, target: "/api/admin/v1/user/info?id=" + id}},
	}
}

// TestHackatimeContract pins status and body of every Hackatime-compatible
// call. Run with -update to rewrite the goldens.
func TestHackatimeContract(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, tc := range contractCases(f) {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, tc.c)
			got := renderGolden(t, tc, rec.Code, rec.Body.Bytes())
			path := filepath.Join("testdata", "hackatime", tc.name+".golden")
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path) //nolint:gosec // test fixture path
			if err != nil {
				t.Fatalf("reading golden (run with -update): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("response changed for %s\n--- got ---\n%s\n--- want ---\n%s", tc.name, got, want)
			}
		})
	}
}

// volatile matches values that differ between runs.
var volatile = regexp.MustCompile(`"(created_at|updated_at|fields_hash|trust_checked_at)": "[^"]*"`)

func renderGolden(t *testing.T, tc contractCase, status int, body []byte) []byte {
	t.Helper()
	var b strings.Builder
	method := tc.c.method
	if method == "" {
		method = http.MethodGet
	}
	fmt.Fprintf(&b, "# Hackatime %s\n# %s %s\n", tc.source, method, tc.c.target)
	if tc.c.body != "" {
		fmt.Fprintf(&b, "# body: %s\n", tc.c.body)
	}
	if tc.c.form != "" {
		fmt.Fprintf(&b, "# form: %s\n", tc.c.form)
	}
	fmt.Fprintf(&b, "status: %d\n\n", status)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(body), "", "  "); err != nil {
		t.Fatalf("body is not JSON: %q", body)
	}
	b.Write(volatile.ReplaceAll(pretty.Bytes(), []byte(`"$1": "<volatile>"`)))
	b.WriteByte('\n')
	return []byte(b.String())
}

// keysOf returns an object's keys in the order they were sent.
func keysOf(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := d.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// Hackatime's keys at 43bde10, in the order it renders them
// (app/controllers/concerns/api/admin/v1/user_utilities.rb), and the
// trailing "logbook" object Logbook adds where it adds one.
var hackatimeKeys = map[string][]string{
	"info": {"id", "username", "display_name", "slack_uid", "slack_username", "github_username", "timezone",
		"country_code", "admin_level", "trust_level", "suspected", "banned", "created_at", "updated_at",
		"last_heartbeat_at", "email_addresses", "api_keys_count", "stats", "logbook"},
	"info.stats": {"total_heartbeats", "total_coding_time", "languages_used", "projects_worked_on", "days_active"},
	// as_json(only: [...], methods: [:display_name, :avatar_url]): columns,
	// then methods.
	"info_batch": {"id", "username", "slack_uid", "slack_username", "github_username", "timezone", "country_code",
		"trust_level", "slack_avatar_url", "github_avatar_url", "display_name", "avatar_url", "logbook"},
	"heartbeats":   {"user_id", "heartbeats", "total_count", "has_more"},
	"heartbeat":    {"id", "time", "created_at", "lineno", "cursorpos", "is_write", "project", "language", "entity", "branch", "category", "dependencies", "editor", "machine", "operating_system", "type", "project_root_count", "user_agent", "line_additions", "line_deletions", "ip_address", "lines", "source_type", "hidden", "ja4", "logbook"},
	"heartbeat_ai": {"id", "time", "created_at", "lineno", "cursorpos", "is_write", "project", "language", "entity", "branch", "category", "dependencies", "editor", "machine", "operating_system", "type", "project_root_count", "user_agent", "line_additions", "line_deletions", "ip_address", "lines", "source_type", "ai_model", "ai_session", "ai_subscription_plan", "ai_input_tokens", "ai_output_tokens", "ai_prompt_length", "ai_line_changes", "human_line_changes", "hidden", "ja4", "logbook"},
	"projects":     {"user_id", "username", "projects", "total_projects"},
	"project":      {"name", "total_heartbeats", "total_duration", "first_heartbeat", "last_heartbeat", "languages", "repo", "repo_mapping_id", "archived", "logbook"},
	"trust_log":    {"id", "previous_trust_level", "new_trust_level", "changed_by", "reason", "notes", "created_at", "edited_at", "edited_by", "original_reason", "original_notes", "logbook"},
	"search_user":  {"id", "username", "slack_username", "github_username", "slack_avatar_url", "github_avatar_url", "email", "rank_score"},
}

// TestHackatimeKeyOrder checks every compatible response's objects carry
// exactly Hackatime's keys, in its order.
func TestHackatimeKeyOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := strconv.FormatInt(f.orpheusID, 10)
	get := func(target string) map[string]json.RawMessage {
		return decode[map[string]json.RawMessage](t, f.expect(t, call{target: target}, http.StatusOK))
	}
	want := func(name string, raw json.RawMessage) {
		t.Helper()
		if got := keysOf(t, raw); !slices.Equal(got, hackatimeKeys[name]) {
			t.Fatalf("%s keys = %v\nwant %v", name, got, hackatimeKeys[name])
		}
	}
	first := func(raw json.RawMessage) json.RawMessage {
		t.Helper()
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
			t.Fatalf("not a non-empty list: %s", raw)
		}
		return list[0]
	}

	info := get("/api/admin/v1/user/info?id=" + id)
	want("info", info["user"])
	want("info.stats", decode[map[string]json.RawMessage](t, info["user"])["stats"])
	want("info_batch", first(get("/api/admin/v1/user/info_batch?ids=" + id)["users"]))

	hb := f.expect(t, call{target: "/api/admin/v1/user/heartbeats?id=" + id}, http.StatusOK)
	want("heartbeats", hb)
	want("heartbeat", first(decode[map[string]json.RawMessage](t, hb)["heartbeats"]))
	want("heartbeat_ai", first(get("/api/admin/v1/user/heartbeats?include_ai=true&id=" + id)["heartbeats"]))

	pr := f.expect(t, call{target: "/api/admin/v1/user/projects?id=" + id}, http.StatusOK)
	want("projects", pr)
	want("project", first(decode[map[string]json.RawMessage](t, pr)["projects"]))
	want("trust_log", first(get("/api/admin/v1/user/trust_logs?id=" + id)["trust_logs"]))

	s := f.expect(t, call{method: http.MethodPost, target: "/api/admin/v1/user/search_fuzzy", body: `{"query": "orpheus"}`}, http.StatusOK)
	want("search_user", first(decode[map[string]json.RawMessage](t, s)["users"]))
}
