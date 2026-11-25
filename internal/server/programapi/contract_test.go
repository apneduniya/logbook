package programapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/hackatime/*.golden")

// contractCase is one request a program repo makes (research section 2,
// docs/api/programs.md), pinned with its full response.
type contractCase struct {
	name string // golden file name
	who  string // the program(s) that send it
	c    call
}

// contractCases builds the requests. {id} is orpheus's public id; pat is a
// private user.
func contractCases(f *fixture) []contractCase {
	id := strconv.FormatInt(f.orpheusID, 10)
	return []contractCase{
		// OAuth link: every OAuth program reads me after the token exchange.
		{"authenticated_me", "16 OAuth programs (Stardance, Flavortown, Save Point, 3am-ysws, ...)",
			call{target: "/api/v1/authenticated/me", bearer: "orpheus-profile-read"}},
		{"authenticated_me_unlinked", "any OAuth program, user without a Hackatime link",
			call{target: "/api/v1/authenticated/me", bearer: "pat-read-app2"}},
		// The project picker.
		{"authenticated_projects", "3am-ysws, Re-Dream, cq, disposition, fellow-ysws, hackrail, macondo, pixl, shrink, the-carnival, twisted",
			call{target: "/api/v1/authenticated/projects", bearer: "orpheus-profile-read"}},
		{"authenticated_projects_archived_raw", "the project picker with include_archived and raw=true",
			call{target: "/api/v1/authenticated/projects?include_archived=true&raw=true", bearer: "orpheus-profile-read"}},
		// Stardance (app/services/hackatime_service.rb).
		{"stardance_my_stats", "Stardance: users/my/stats with the user's API key",
			call{target: "/api/v1/users/my/stats?features=projects&start_date=2026-05-31&test_param=true&no_ai_coding=false", bearer: orpheusKey}},
		{"stardance_stats", "Stardance: the public fallback",
			call{target: "/api/v1/users/" + id + "/stats?features=projects&start_date=2026-05-31&test_param=true&no_ai_coding=false"}},
		{"stardance_project_total", "Stardance: hours for the Hackatime projects of one Stardance project",
			call{target: "/api/v1/users/" + id + "/stats?test_param=true&total_seconds=true&filter_by_project[]=logbook&filter_by_project[]=site&start_date=2026-05-31&no_ai_coding=false"}},
		{"stardance_no_ai_coding", "Stardance: no_ai_coding=true",
			call{target: "/api/v1/users/" + id + "/stats?test_param=true&total_seconds=true&filter_by_project[]=logbook&start_date=2026-05-31&no_ai_coding=true"}},
		{"stardance_trust_factor", "Stardance and 17 more repos that read trust",
			call{target: "/api/v1/users/" + id + "/trust_factor"}},
		{"stardance_spans", "Stardance: spans for reviewers",
			call{target: "/api/v1/users/" + id + "/heartbeats/spans?start_date=2026-06-10&end_date=2026-06-11&project[]=logbook&project[]=site"}},
		{"stardance_my_heartbeats", "Stardance: checking that Lookout time arrived",
			call{target: "/api/v1/my/heartbeats?start_time=2026-06-10T10:00:00Z&end_time=2026-06-10T10:59:59Z", bearer: orpheusKey}},
		// Flavortown (app/services/hackatime_service.rb).
		{"flavortown_stats", "Flavortown: public stats with projects",
			call{target: "/api/v1/users/" + id + "/stats?features=projects&start_date=2025-12-15&test_param=true"}},
		{"flavortown_devlog", "Flavortown: per-devlog total",
			call{target: "/api/v1/users/" + id + "/stats?total_seconds=true&filter_by_project=logbook,site&start_date=2026-06-01&end_date=2026-06-30"}},
		// Save Point.
		{"savepoint_category", "Save Point: filter_by_category",
			call{target: "/api/v1/users/" + id + "/stats?total_seconds=true&filter_by_project=logbook&filter_by_category=coding&start_date=2026-06-01"}},
		// Crossroads (Go, backend/hackatime/hackatime.go): by username here,
		// since Slack ids aren't supported.
		{"crossroads_stats", "Crossroads: stats by username",
			call{target: "/api/v1/users/orpheus/stats?start_date=2026-06-01T00:00:00Z&end_date=2026-06-30T00:00:00Z&filter_by_project=logbook&features=projects"}},
		{"crossroads_spans", "Crossroads: spans by username",
			call{target: "/api/v1/users/orpheus/heartbeats/spans?start_date=2026-06-01&end_date=2026-06-30&filter_by_project=logbook"}},
		{"crossroads_slack_id", "Crossroads: a Slack id (not supported in V1)",
			call{target: "/api/v1/users/U01ABCDEF12/stats"}},
		// Ruby clients that put a Time in the query send Time#to_s.
		{"ruby_time_to_s", "Ruby programs passing Time values (HTTParty, Faraday, to_query)",
			call{target: "/api/v1/users/" + id + "/stats?total_seconds=true&filter_by_project=logbook&start_date=2026-06-01+00:00:00+UTC&end_date=2026-06-30T00:00:00%2B0000"}},
		{"ruby_time_to_s_spans", "spans with Time#to_s bounds",
			call{target: "/api/v1/users/" + id + "/heartbeats/spans?start_date=2026-06-10+00:00:00+UTC&end_date=2026-06-11+00:00:00+%2B0000&project=logbook"}},
		// A list of only separators is an empty filter: nothing matches.
		{"empty_filter_total", "filter_by_project of only commas",
			call{target: "/api/v1/users/" + id + "/stats?total_seconds=true&filter_by_project=,&start_date=2026-06-01"}},
		{"empty_filter_projects", "features=projects with filter_by_project of only commas",
			call{target: "/api/v1/users/" + id + "/stats?features=projects&filter_by_project=,&start_date=2026-06-01"}},
		{"empty_filter_spans", "spans with filter_by_project of only commas",
			call{target: "/api/v1/users/" + id + "/heartbeats/spans?start_date=2026-06-01&filter_by_project=,"}},
		// Ari's oracle and per-project reads.
		{"ari_projects_details", "Ari, Stardance: projects/details",
			call{target: "/api/v1/users/" + id + "/projects/details"}},
		{"user_project", "users/{id}/project/{name}",
			call{target: "/api/v1/users/" + id + "/project/logbook"}},
		{"user_projects", "users/{id}/projects",
			call{target: "/api/v1/users/" + id + "/projects"}},
		// Default stats (languages), raw=true and a limit.
		{"stats_default", "stats with Hackatime's defaults",
			call{target: "/api/v1/users/orpheus/stats"}},
		{"stats_raw_limit", "stats with raw=true and limit=1",
			call{target: "/api/v1/users/orpheus/stats?features=languages,projects&raw=true&limit=1"}},
		// Errors.
		{"private_profile", "any program, private user without a token",
			call{target: "/api/v1/users/pat/stats"}},
		{"invalid_start_date", "a bad date",
			call{target: "/api/v1/users/orpheus/stats?start_date=yesterday"}},
	}
}

// TestHackatimeContract pins status, caching header and body of every
// request programs make. Run with -update to rewrite the goldens.
func TestHackatimeContract(t *testing.T) {
	f := newFixture(t)
	for _, tc := range contractCases(f) {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, tc.c)
			got := renderGolden(t, tc, rec.Code, rec.Header().Get("Cache-Control"), rec.Body.Bytes())
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

// volatile matches values that differ between runs: database timestamps.
var volatile = regexp.MustCompile(`"(created_at|updated_at|fields_hash)": "[^"]*"`)

func renderGolden(t *testing.T, tc contractCase, status int, cache string, body []byte) []byte {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n# GET %s\n", tc.who, tc.c.target)
	if tc.c.bearer != "" {
		fmt.Fprintf(&b, "# Authorization: Bearer <%s>\n", credName(tc.c.bearer))
	}
	fmt.Fprintf(&b, "status: %d\ncache-control: %s\n\n", status, cache)
	if len(body) > 0 {
		// Indent the body as sent, keeping its key order.
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, bytes.TrimSpace(body), "", "  "); err != nil {
			t.Fatalf("body is not JSON: %q", body)
		}
		b.Write(volatile.ReplaceAll(pretty.Bytes(), []byte(`"$1": "<volatile>"`)))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func credName(s string) string {
	if s == orpheusKey {
		return "orpheus API key"
	}
	return s + " token"
}
