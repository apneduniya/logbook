package router_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/apneduniya/logbook/internal/server/store/seedtest"
)

// TestAdminAPIRefusesEveryOtherCredential sends the admin API every
// credential the server issues, on the real handler: only an admin key of
// an admin reads anything. An admin's own session, user API key and OAuth
// token are refused like anyone's, and so are a revoked key and the key of
// a demoted admin (ADR 0022).
func TestAdminAPIRefusesEveryOtherCredential(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	fiona, orpheus := s.signIn("fiona"), s.signIn("orpheus")
	s.seedActivity(orpheus.id)
	target := "/api/admin/v1/user/info?id=" + strconv.FormatInt(seedtest.PublicID(t, s.st, orpheus.id), 10)

	if _, err := s.adminKeys.Grant(ctx, "fiona"); err != nil {
		t.Fatal(err)
	}
	key, err := s.adminKeys.CreateKey(ctx, "fiona", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	s.expect(request{target: target, bearer: key.Key}, http.StatusOK)
	// No CORS on the admin API.
	rec := s.do(request{target: target, bearer: key.Key, origin: webOrigin})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("the admin API answered with CORS headers")
	}

	// fiona's other credentials: a session, a user API key, an OAuth token.
	apiKey := decode[map[string]any](t, s.expect(request{method: http.MethodPost, target: "/api/v1/api_keys",
		browser: fiona, body: `{"name": "editor"}`}, http.StatusCreated))["key"].(string)
	stardance := s.registerApp(fiona, "stardance")
	tok := s.exchange(stardance, s.authorize(fiona, stardance, "profile read"))
	clientTok := decode[token](t, s.expect(request{method: http.MethodPost, target: "/oauth/token",
		form: url.Values{"grant_type": {"client_credentials"}, "scope": {"submissions"}}, basic: stardance.creds()}, http.StatusOK))

	refused := map[string]request{
		"none":                         {target: target},
		"session":                      {target: target, browser: fiona},
		"session and CSRF on a POST":   {method: http.MethodPost, target: "/api/admin/v1/user/search_fuzzy", browser: fiona, body: `{"query": "o"}`},
		"user API key (Bearer)":        {target: target, bearer: apiKey},
		"user API key (Basic)":         {target: target, authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte(apiKey))},
		"user API key (query)":         {target: target + "&api_key=" + apiKey},
		"OAuth access token":           {target: target, bearer: tok.AccessToken},
		"OAuth refresh token":          {target: target, bearer: tok.RefreshToken},
		"client-credentials token":     {target: target, bearer: clientTok.AccessToken},
		"OAuth client secret":          {target: target, bearer: stardance.ClientSecret},
		"admin key in a cookie":        {target: target, authorization: "Cookie " + key.Key},
		"admin key prefix only":        {target: target, bearer: key.Prefix},
		"admin key, Bearer misspelled": {target: target, authorization: "Bearr " + key.Key},
	}
	for name, rq := range refused {
		t.Run(name, func(t *testing.T) {
			rec := s.do(rq)
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Access denied") {
				t.Fatalf("= %d %s, want 401", rec.Code, rec.Body)
			}
		})
	}

	// A revoked key, then a demoted admin's key.
	spare, err := s.adminKeys.CreateKey(ctx, "fiona", "spare")
	if err != nil {
		t.Fatal(err)
	}
	s.expect(request{target: target, bearer: spare.Key}, http.StatusOK)
	if err := s.adminKeys.RevokeKey(ctx, spare.Prefix); err != nil {
		t.Fatal(err)
	}
	s.expect(request{target: target, bearer: spare.Key}, http.StatusUnauthorized)
	if _, err := s.adminKeys.Revoke(ctx, "fiona"); err != nil {
		t.Fatal(err)
	}
	s.expect(request{target: target, bearer: key.Key}, http.StatusUnauthorized)

	// A non-admin's credentials never reach it either.
	s.expect(request{target: target, browser: orpheus}, http.StatusUnauthorized)
}
