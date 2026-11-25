package router_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// seedSubmission stores a ready submission of user addressed to app, as the
// submissions package would, and returns its id.
func (s *server) seedSubmission(user uuid.UUID, appID string) uuid.UUID {
	s.t.Helper()
	id, token := uuid.New(), "e2e-token-"+uuid.NewString()
	enc, err := s.box.Seal([]byte(token), id[:])
	if err != nil {
		s.t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(token))
	if _, err := s.st.Pool.Exec(context.Background(), `
		INSERT INTO submissions (id, user_id, project, from_day, to_day, program_app_id, status, token_hash, token_enc, created_at)
		VALUES ($1, $2, 'logbook', '2026-06-10', '2026-06-10', $3, 'ready', $4, $5, $6)`,
		id, user, appID, hash[:], enc, now); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// A program reads the submissions addressed to it with its own
// client-credentials token, and nothing else reaches them: another app's
// token, a user's token, an API key or a session.
func TestProgramReadsOnlyItsSubmissions(t *testing.T) {
	s := newServer(t)
	owner, other, orpheus := s.signIn("fiona"), s.signIn("mallory"), s.signIn("orpheus")
	terra, rival := s.registerApp(owner, "terra"), s.registerApp(other, "rival")
	id := s.seedSubmission(orpheus.id, terra.ID)

	clientToken := func(a app) string {
		form := url.Values{"grant_type": {"client_credentials"}, "scope": {"submissions"}}
		return decode[token](t, s.expect(request{method: http.MethodPost, target: "/oauth/token", form: form, basic: a.creds()}, http.StatusOK)).AccessToken
	}
	terraTok, rivalTok := clientToken(terra), clientToken(rival)
	userTok := s.exchange(terra, s.authorize(orpheus, terra, "profile read")).AccessToken
	key := decode[map[string]any](t, s.expect(request{method: http.MethodPost, target: "/api/v1/api_keys", browser: orpheus, body: `{"name": "laptop"}`}, http.StatusCreated))["key"].(string)

	list := decode[map[string][]map[string]any](t, s.expect(request{target: "/api/v1/submissions", bearer: terraTok}, http.StatusOK))["submissions"]
	if len(list) != 1 || list[0]["id"] != id.String() || !strings.HasPrefix(list[0]["review_url"].(string), webOrigin+"/r/e2e-token-") {
		t.Fatalf("terra's list = %v", list)
	}
	s.expect(request{target: "/api/v1/submissions/" + id.String(), bearer: terraTok}, http.StatusOK)

	if got := decode[map[string][]map[string]any](t, s.expect(request{target: "/api/v1/submissions", bearer: rivalTok}, http.StatusOK))["submissions"]; len(got) != 0 {
		t.Fatalf("rival's list = %v", got)
	}
	for _, tc := range []struct {
		name string
		rq   request
		want int
	}{
		{"another app's token", request{target: "/api/v1/submissions/" + id.String(), bearer: rivalTok}, http.StatusNotFound},
		{"a user's token", request{target: "/api/v1/submissions", bearer: userTok}, http.StatusForbidden},
		{"an API key", request{target: "/api/v1/submissions", bearer: key}, http.StatusUnauthorized},
		{"a session", request{target: "/api/v1/submissions", browser: owner}, http.StatusUnauthorized},
		{"nothing", request{target: "/api/v1/submissions/" + id.String()}, http.StatusUnauthorized},
		// The members' inbox takes a session of a member only.
		{"the owner's session", request{target: "/api/v1/me/apps/" + terra.ID + "/submissions", browser: owner}, http.StatusOK},
		{"a non-member's session", request{target: "/api/v1/me/apps/" + terra.ID + "/submissions", browser: other}, http.StatusNotFound},
		{"the program's token on the inbox", request{target: "/api/v1/me/apps/" + terra.ID + "/submissions", bearer: terraTok}, http.StatusUnauthorized},
		// The submitter's own list takes a session, not a token.
		{"a token on me/submissions", request{target: "/api/v1/me/submissions", bearer: userTok}, http.StatusUnauthorized},
		{"the submitter's session", request{target: "/api/v1/me/submissions/" + id.String(), browser: orpheus}, http.StatusOK},
		{"another user's session", request{target: "/api/v1/me/submissions/" + id.String(), browser: other}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := s.do(tc.rq); rec.Code != tc.want {
				t.Fatalf("= %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
}
