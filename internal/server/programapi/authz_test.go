package programapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// userReadPaths are the stats-controller calls guarded like stats.
func userReadPaths(id string) []string {
	return []string{
		"/api/v1/users/" + id + "/stats",
		"/api/v1/users/" + id + "/heartbeats/spans",
		"/api/v1/users/" + id + "/projects",
		"/api/v1/users/" + id + "/project/diary",
		"/api/v1/users/" + id + "/projects/details",
	}
}

// TestPrivateProfile: a private user's data needs a read credential of that
// same user. Without one every stats-controller call is Hackatime's 403; a
// token or key of another user (from any app) doesn't help, and neither does
// the user's own token without the read scope.
func TestPrivateProfile(t *testing.T) {
	f := newFixture(t)
	denied := []struct {
		name string
		c    call
	}{
		{"no credentials", call{}},
		{"public user's read token", call{bearer: "orpheus-profile-read"}},
		{"public user's API key", call{bearer: orpheusKey}},
		{"own profile-only token", call{bearer: "pat-profile"}},
		{"own API key over Basic (Hackatime takes Bearer only here)", call{basic: patKey}},
		{"unknown token", call{bearer: "nope"}},
	}
	for _, id := range []string{"pat", fmt.Sprint(f.patID)} {
		for _, path := range userReadPaths(id) {
			for _, tc := range denied {
				t.Run(tc.name+" "+path, func(t *testing.T) {
					tc.c.target = path
					rec := f.do(t, tc.c)
					if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != `{"error":"user has disabled public stats"}` {
						t.Fatalf("got %d %s, want Hackatime's 403", rec.Code, rec.Body)
					}
				})
			}
			for _, c := range []call{{bearer: "pat-read-app2"}, {bearer: patKey}} {
				c.target = path
				if rec := f.do(t, c); rec.Code != http.StatusOK {
					t.Errorf("%s with pat's own credential: %d %s", path, rec.Code, rec.Body)
				}
			}
		}
	}
	// "my" reads the token's own user, never anyone else.
	rec := f.do(t, call{target: "/api/v1/users/my/stats?total_seconds=true", bearer: "pat-read-app2"})
	if got := decode(t, rec); rec.Code != http.StatusOK || got["credited_seconds"] != float64(600) {
		t.Fatalf("pat's my/stats = %d %v, want her 600 counted seconds", rec.Code, got)
	}
	rec = f.do(t, call{target: "/api/v1/users/my/stats?total_seconds=true", bearer: "orpheus-read"})
	if got := decode(t, rec); got["credited_seconds"] != float64(1470) {
		t.Fatalf("orpheus's my/stats = %v, want his own 1470", got)
	}
}

// TestPublicProfileReadsNeedNoToken: public stats need no credentials, and a
// bad token doesn't turn a public read into an error (Hackatime ignores it).
func TestPublicProfileReadsNeedNoToken(t *testing.T) {
	f := newFixture(t)
	for _, path := range userReadPaths("orpheus") {
		if strings.HasSuffix(path, "/project/diary") {
			path = strings.TrimSuffix(path, "diary") + "logbook"
		}
		for _, c := range []call{{}, {bearer: "nope"}, {bearer: "pat-read-app2"}} {
			c.target = path
			if rec := f.do(t, c); rec.Code != http.StatusOK {
				t.Errorf("%s (%+v) = %d %s", path, c, rec.Code, rec.Body)
			}
		}
	}
}

// TestMyNeedsReadCredential: "my" with no credential, or one without the
// read scope, names nobody.
func TestMyNeedsReadCredential(t *testing.T) {
	f := newFixture(t)
	for _, c := range []call{{}, {bearer: "orpheus-profile"}, {basic: orpheusKey}, {bearer: "banned-read"}} {
		c.target = "/api/v1/users/my/stats"
		rec := f.do(t, c)
		if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != `{"error":"User not found"}` {
			t.Errorf("%+v: %d %s, want 404 User not found", c, rec.Code, rec.Body)
		}
	}
	// "my" is not an alias on trust_factor (Hackatime's set_user doesn't run there).
	if rec := f.do(t, call{target: "/api/v1/users/my/trust_factor", bearer: "orpheus-read"}); rec.Code != http.StatusNotFound {
		t.Errorf("my/trust_factor = %d, want 404", rec.Code)
	}
}

// TestAuthenticatedMe pins Doorkeeper's checks on authenticated/me: a Bearer
// token with the profile scope.
func TestAuthenticatedMe(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name  string
		c     call
		want  int
		error string // WWW-Authenticate error
	}{
		{"profile token", call{bearer: "orpheus-profile"}, http.StatusOK, ""},
		{"profile read token", call{bearer: "orpheus-profile-read"}, http.StatusOK, ""},
		{"read-only token", call{bearer: "orpheus-read"}, http.StatusForbidden, "insufficient_scope"},
		{"no token", call{}, http.StatusUnauthorized, "invalid_token"},
		{"unknown token", call{bearer: "nope"}, http.StatusUnauthorized, "invalid_token"},
		{"API key", call{bearer: orpheusKey}, http.StatusUnauthorized, "invalid_token"},
		{"token in the query", call{target: "/api/v1/authenticated/me?access_token=orpheus-profile"}, http.StatusUnauthorized, "invalid_token"},
		{"red user still reads me", call{bearer: "banned-read"}, http.StatusOK, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.c.target == "" {
				tc.c.target = "/api/v1/authenticated/me"
			}
			rec := f.do(t, tc.c)
			if rec.Code != tc.want {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.error != "" {
				if h := rec.Header().Get("WWW-Authenticate"); !strings.Contains(h, `error="`+tc.error+`"`) {
					t.Fatalf("WWW-Authenticate = %q, want error=%q", h, tc.error)
				}
				if rec.Body.Len() != 0 {
					t.Fatalf("body = %q, want empty (Doorkeeper's head)", rec.Body)
				}
			}
		})
	}
	// Each token reads its own user.
	if got := decode(t, f.do(t, call{target: "/api/v1/authenticated/me", bearer: "pat-profile"})); got["id"] != float64(f.patID) {
		t.Fatalf("pat's me = %v, want id %d", got, f.patID)
	}
}

// TestAuthenticatedProjects: the read scope is required, each token lists
// only its own user's projects, and a red user is refused.
func TestAuthenticatedProjects(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(t, call{target: "/api/v1/authenticated/projects", bearer: "orpheus-profile"}); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Fatalf("profile-only token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if rec := f.do(t, call{target: "/api/v1/authenticated/projects", bearer: orpheusKey}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("API key: %d, want 401 (Doorkeeper only)", rec.Code)
	}
	rec := f.do(t, call{target: "/api/v1/authenticated/projects", bearer: "banned-read"})
	if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != `{"error":"Unauthorized"}` {
		t.Fatalf("red user: %d %s", rec.Code, rec.Body)
	}
	names := func(token string) []string {
		rec := f.do(t, call{target: "/api/v1/authenticated/projects", bearer: token})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", token, rec.Code, rec.Body)
		}
		var out []string
		for _, p := range decode(t, rec)["projects"].([]any) {
			out = append(out, p.(map[string]any)["name"].(string))
		}
		return out
	}
	if got := fmt.Sprint(names("pat-read-app2")); got != "[diary]" {
		t.Fatalf("pat's projects = %s, want [diary]", got)
	}
	// Counted time first; hidden and archived projects left out.
	if got := fmt.Sprint(names("orpheus-read")); got != "[logbook agent-only site]" {
		t.Fatalf("orpheus's projects = %s", got)
	}
}

// TestMyHeartbeatsAuth: an API key (Bearer or Basic) or a read token; each
// reads only its own user's heartbeats.
func TestMyHeartbeatsAuth(t *testing.T) {
	f := newFixture(t)
	const path = "/api/v1/my/heartbeats?start_time=2026-06-10T00:00:00Z&end_time=2026-06-10T23:59:59Z"
	ok := []call{{bearer: patKey}, {basic: patKey}, {bearer: "pat-read-app2"}}
	for _, c := range ok {
		c.target = path
		rec := f.do(t, c)
		if rec.Code != http.StatusOK {
			t.Fatalf("%+v: %d %s", c, rec.Code, rec.Body)
		}
		hbs := decode(t, rec)["heartbeats"].([]any)
		if len(hbs) != 2 {
			t.Fatalf("%+v: %d heartbeats, want pat's 2", c, len(hbs))
		}
		for _, h := range hbs {
			if h.(map[string]any)["user_id"] != float64(f.patID) {
				t.Fatalf("heartbeat of another user: %v", h)
			}
		}
	}
	for _, c := range []call{{}, {bearer: "pat-profile"}, {bearer: bannedKey}, {bearer: "banned-read"}, {bearer: "nope"}, {basic: "nope"}} {
		c.target = path
		rec := f.do(t, c)
		if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != `{"error":"Unauthorized"}` {
			t.Errorf("%+v: %d %s, want 401", c, rec.Code, rec.Body)
		}
	}
}

// TestHiddenProjectsAreInvisible: a hidden project shows up nowhere, not
// even for its owner's own token.
func TestHiddenProjectsAreInvisible(t *testing.T) {
	f := newFixture(t)
	paths := []string{
		"/api/v1/users/orpheus/stats?features=projects,languages",
		"/api/v1/users/orpheus/projects?start_date=2026-06-01",
		"/api/v1/users/orpheus/projects/details",
		"/api/v1/users/orpheus/heartbeats/spans?project=secret",
		"/api/v1/authenticated/projects",
		"/api/v1/my/heartbeats?start_time=2026-06-10T00:00:00Z&end_time=2026-06-10T23:59:59Z",
	}
	for _, p := range paths {
		rec := f.do(t, call{target: p, bearer: "orpheus-profile-read"})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body)
		}
		if body := rec.Body.String(); strings.Contains(body, "secret") || strings.Contains(body, "Rust") {
			t.Errorf("%s leaks the hidden project: %s", p, body)
		}
	}
	if rec := f.do(t, call{target: "/api/v1/users/orpheus/project/secret"}); rec.Code != http.StatusNotFound {
		t.Errorf("project/secret = %d, want 404", rec.Code)
	}
	rec := f.do(t, call{target: "/api/v1/users/orpheus/stats?total_seconds=true&filter_by_project=secret"})
	if got := decode(t, rec); got["raw_seconds"] != float64(0) || got["credited_seconds"] != float64(0) {
		t.Errorf("filtering on the hidden project = %v, want zeros", got)
	}
}
