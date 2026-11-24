package programapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/httpapi"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
)

// TestIdentifierResolution pins docs/api/programs.md "User ids": digits are
// a public id first and a numeric username second; anything else is a
// username, case-insensitively. Slack and HCA ids aren't supported.
func TestIdentifierResolution(t *testing.T) {
	f := newFixture(t)
	// A user whose username is orpheus's public id: the public id wins.
	seedtest.User(t, f.st, fmt.Sprint(f.orpheusID), "UTC", true)
	tests := []struct {
		ident  string
		wantID int64 // public id in data.user_id; 0 means 404
	}{
		{fmt.Sprint(f.orpheusID), f.orpheusID},
		{"orpheus", f.orpheusID},
		{"ORPHEUS", f.orpheusID},
		{"42", seedtest.PublicID(t, f.st, f.numeric)},
		{"0000042", 0},              // no public id 42, and no user named "0000042"
		{"99999999999999999999", 0}, // too big for a public id, too long for a username
		{"U01ABCDEF12", 0},          // a Slack id
		{"nobody", 0},
		{"a.b", 0},
	}
	for _, tc := range tests {
		t.Run(tc.ident, func(t *testing.T) {
			rec := f.do(t, call{target: "/api/v1/users/" + tc.ident + "/stats"})
			if tc.wantID == 0 {
				if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != `{"error":"User not found"}` {
					t.Fatalf("got %d %s, want 404", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d %s", rec.Code, rec.Body)
			}
			data := decode(t, rec)["data"].(map[string]any)
			if data["user_id"] != fmt.Sprint(tc.wantID) {
				t.Fatalf("user_id = %v, want %d", data["user_id"], tc.wantID)
			}
		})
	}
}

// TestTrustFactorMasking: yellow reads as blue; unlinked and unknown read as
// blue with trust_verified false; red stays red.
func TestTrustFactorMasking(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		user     string
		level    string
		value    float64
		verified bool
	}{
		{"orpheus", "green", 2, true},
		{"yellowy", "blue", 0, true},
		{"unknowny", "blue", 0, false},
		{"pat", "blue", 0, false}, // unlinked; trust_factor is public even for private users
		{"banned", "red", 1, true},
	}
	for _, tc := range tests {
		for _, path := range []string{"/api/v1/users/" + tc.user + "/trust_factor", "/api/v1/users/" + tc.user + "/stats"} {
			c := call{target: path}
			if tc.user == "pat" && strings.HasSuffix(path, "/stats") {
				c.bearer = patKey
			}
			rec := f.do(t, c)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
			}
			body := decode(t, rec)
			tf := body
			if strings.HasSuffix(path, "/stats") {
				tf = body["trust_factor"].(map[string]any)
			}
			ext := body["logbook"].(map[string]any)
			if tf["trust_level"] != tc.level || tf["trust_value"] != tc.value || ext["trust_verified"] != tc.verified {
				t.Errorf("%s: %v / %v, want %s %v verified=%v", path, tf, ext, tc.level, tc.value, tc.verified)
			}
		}
	}
	if rec := f.do(t, call{target: "/api/v1/users/nobody/trust_factor"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user trust_factor = %d", rec.Code)
	}
}

// TestCountedVersusRaw: total_seconds is counted time unless raw=true, and a
// user who never ran the agent gets 0 counted seconds, not their raw time.
func TestCountedVersusRaw(t *testing.T) {
	f := newFixture(t)
	get := func(target string) map[string]any {
		t.Helper()
		rec := f.do(t, call{target: target})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body)
		}
		return decode(t, rec)
	}
	if got := get("/api/v1/users/orpheus/stats?total_seconds=true"); got["total_seconds"] != float64(1470) || got["raw_seconds"] != float64(3000) {
		t.Errorf("counted = %v", got)
	}
	if got := get("/api/v1/users/orpheus/stats?total_seconds=true&raw=true"); got["total_seconds"] != float64(3000) {
		t.Errorf("raw = %v", got)
	}
	got := get("/api/v1/users/nodevice/stats?total_seconds=true")
	if got["total_seconds"] != float64(0) || got["credited_seconds"] != float64(0) || got["raw_seconds"] != float64(420) {
		t.Errorf("no device = %v, want 0 counted and 420 raw", got)
	}
	// filter_by_category changes only raw figures, and only on this path.
	got = get("/api/v1/users/orpheus/stats?total_seconds=true&filter_by_category=browsing")
	if got["credited_seconds"] != float64(1470) || got["raw_seconds"] != float64(300) {
		t.Errorf("filter_by_category = %v", got)
	}
}

// TestRateLimit: valid credentials get their own bucket; everything else
// shares the client's.
func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	mux := f.newMux(t, httpapi.NewRateLimiter(1.0/3600, 2, 100, time.Hour))
	for range 2 {
		if rec := doOn(t, mux, call{target: "/api/v1/users/orpheus/trust_factor"}); rec.Code != http.StatusOK {
			t.Fatalf("within burst: %d", rec.Code)
		}
	}
	rec := doOn(t, mux, call{target: "/api/v1/users/orpheus/stats", bearer: "made-up"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the client budget with a made-up token: %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := doOn(t, mux, call{target: "/api/v1/authenticated/me", bearer: "orpheus-profile"}); rec.Code != http.StatusOK {
		t.Fatalf("a valid token has its own budget: %d", rec.Code)
	}
	if rec := doOn(t, mux, call{target: "/api/v1/users/orpheus/trust_factor", remote: "198.51.100.7:1"}); rec.Code != http.StatusOK {
		t.Fatalf("another client has its own budget: %d", rec.Code)
	}
}

// TestBadParams: Hackatime's 422 for unparsable dates.
func TestBadParams(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{
		"/api/v1/users/orpheus/stats?end_date=never",
		"/api/v1/users/orpheus/heartbeats/spans?start_date=x",
	} {
		rec := f.do(t, call{target: p})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Invalid ") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
	rec := f.do(t, call{target: "/api/v1/my/heartbeats?start_time=x", bearer: orpheusKey})
	if rec.Code != http.StatusUnprocessableEntity || strings.TrimSpace(rec.Body.String()) != `{"error":"Invalid start_time"}` {
		t.Errorf("my/heartbeats bad time: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, call{target: "/api/v1/users/orpheus/project/nothing-here"}); rec.Code != http.StatusNotFound ||
		strings.TrimSpace(rec.Body.String()) != `{"error":"found nuthin"}` {
		t.Errorf("missing project: %d %s", rec.Code, rec.Body)
	}
}
