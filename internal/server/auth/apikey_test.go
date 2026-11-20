package auth_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/auth"
)

// wakatimeCLIKeyRe is stock wakatime-cli's api_key check (pkg/params/params.go).
var wakatimeCLIKeyRe = regexp.MustCompile("^(waka_)?[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$")

type keyJSON struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Key        string     `json:"key"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

func createKey(t *testing.T, e *env, b browser, name string) keyJSON {
	t.Helper()
	rec := b.send(e, http.MethodPost, "/api/v1/api_keys", fmt.Sprintf(`{"name":%q}`, name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key = %d %s", rec.Code, rec.Body)
	}
	var k keyJSON
	decode(t, rec, &k)
	return k
}

func listKeys(t *testing.T, e *env, b browser) []keyJSON {
	t.Helper()
	rec := b.get(e, "/api/v1/api_keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys = %d", rec.Code)
	}
	var out struct {
		APIKeys []keyJSON `json:"api_keys"`
	}
	decode(t, rec, &out)
	return out.APIKeys
}

func TestCreateAndListAPIKeys(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	k := createKey(t, e, b, "  laptop  ")

	if !wakatimeCLIKeyRe.MatchString(k.Key) {
		t.Errorf("key %q would be rejected by wakatime-cli", k.Key)
	}
	if k.Name != "laptop" || k.Prefix != k.Key[:8] {
		t.Errorf("created key = %+v", k)
	}
	if n := e.count("SELECT count(*) FROM api_keys WHERE key_hash = $1", []byte(k.Key)); n != 0 {
		t.Error("plaintext key stored")
	}
	keys := listKeys(t, e, b)
	if len(keys) != 1 || keys[0].ID != k.ID || keys[0].Key != "" {
		t.Fatalf("list = %+v, want one key without its secret", keys)
	}
	if n := e.count("SELECT count(*) FROM audit_log WHERE action = 'api_key.create' AND subject_id = $1", k.ID); n != 1 {
		t.Errorf("create audit rows = %d", n)
	}
	if n := e.count("SELECT count(*) FROM audit_log WHERE data::text LIKE '%' || $1 || '%'", k.Key); n != 0 {
		t.Error("audit log contains the plaintext key")
	}
}

func TestCreateAPIKeyValidation(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	tests := []struct {
		name, body string
		want       int
	}{
		{"empty name", `{"name":""}`, http.StatusBadRequest},
		{"blank name", `{"name":"   "}`, http.StatusBadRequest},
		{"too long", fmt.Sprintf(`{"name":%q}`, string(make([]byte, 65))), http.StatusBadRequest},
		{"control char", `{"name":"a\u0007b"}`, http.StatusBadRequest},
		{"not json", `name=x`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if rec := b.send(e, http.MethodPost, "/api/v1/api_keys", tc.body); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestAPIKeyLimit(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	var last keyJSON
	for i := range auth.MaxActiveAPIKeys {
		last = createKey(t, e, b, fmt.Sprintf("k%d", i))
	}
	if rec := b.send(e, http.MethodPost, "/api/v1/api_keys", `{"name":"one too many"}`); rec.Code != http.StatusConflict {
		t.Fatalf("over-limit create = %d, want 409", rec.Code)
	}
	if rec := b.send(e, http.MethodDelete, "/api/v1/api_keys/"+last.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	createKey(t, e, b, "fits again")
}

func TestRequireAPIKeyForms(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	k := createKey(t, e, b, "editor")
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	tests := []struct {
		name   string
		target string
		header map[string]string
		want   int
	}{
		{"bearer", "/api/v1/probe/key", map[string]string{"Authorization": "Bearer " + k.Key}, http.StatusOK},
		{"basic key (wakatime-cli)", "/api/v1/probe/key", map[string]string{"Authorization": "Basic " + b64(k.Key)}, http.StatusOK},
		{"basic key colon", "/api/v1/probe/key", map[string]string{"Authorization": "Basic " + b64(k.Key+":")}, http.StatusOK},
		{"basic colon key", "/api/v1/probe/key", map[string]string{"Authorization": "Basic " + b64(":"+k.Key)}, http.StatusOK},
		{"query param", "/api/v1/probe/key?api_key=" + k.Key, nil, http.StatusOK},
		{"none", "/api/v1/probe/key", nil, http.StatusUnauthorized},
		{"unknown key", "/api/v1/probe/key", map[string]string{"Authorization": "Bearer 3f1e2d4c-1111-4222-8333-444455556666"}, http.StatusUnauthorized},
		{"bad format", "/api/v1/probe/key", map[string]string{"Authorization": "Bearer hunter2"}, http.StatusUnauthorized},
		{"bad base64", "/api/v1/probe/key", map[string]string{"Authorization": "Basic !!!"}, http.StatusUnauthorized},
		{"other scheme", "/api/v1/probe/key", map[string]string{"Authorization": "Token " + k.Key}, http.StatusUnauthorized},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Spread across IPs so failure rate limiting doesn't interfere.
			rec := e.do(req{target: tc.target, header: tc.header, remoteAddr: fmt.Sprintf("203.0.113.%d:1", i+1)})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
			if tc.want != http.StatusOK {
				return
			}
			var out struct{ User, Via, ID string }
			decode(t, rec, &out)
			if out.User != b.name || out.Via != "api_key" || out.ID != b.userID {
				t.Fatalf("authenticated as %+v, want %s", out, b.name)
			}
		})
	}
	keys := listKeys(t, e, b)
	if keys[0].LastUsedAt == nil {
		t.Error("last_used_at not recorded")
	}
}

func TestRevokedAPIKeyIsRejected(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	k := createKey(t, e, b, "old")
	auth := map[string]string{"Authorization": "Bearer " + k.Key}
	if rec := e.do(req{target: "/api/v1/probe/key", header: auth}); rec.Code != http.StatusOK {
		t.Fatalf("before revoke = %d", rec.Code)
	}
	if rec := b.send(e, http.MethodDelete, "/api/v1/api_keys/"+k.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if rec := e.do(req{target: "/api/v1/probe/key", header: auth}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after revoke = %d, want 401", rec.Code)
	}
	if rec := b.send(e, http.MethodDelete, "/api/v1/api_keys/"+k.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second revoke = %d, want 404", rec.Code)
	}
	if len(listKeys(t, e, b)) != 0 {
		t.Fatal("revoked key still listed")
	}
	if n := e.count("SELECT count(*) FROM audit_log WHERE action = 'api_key.revoke'"); n != 1 {
		t.Fatalf("revoke audit rows = %d", n)
	}
}

func TestAPIKeysAreScopedToOwner(t *testing.T) {
	e := newEnv(t)
	alice := e.login(fiona)
	mallory := e.login(fakeIdentity{ID: "ident!mallory", FirstName: "Mallory"})
	k := createKey(t, e, alice, "alice laptop")

	if keys := listKeys(t, e, mallory); len(keys) != 0 {
		t.Fatalf("mallory sees %d of alice's keys", len(keys))
	}
	if rec := mallory.send(e, http.MethodDelete, "/api/v1/api_keys/"+k.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user revoke = %d, want 404", rec.Code)
	}
	if rec := mallory.send(e, http.MethodDelete, "/api/v1/api_keys/not-a-uuid", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id = %d, want 404", rec.Code)
	}
	if rec := e.do(req{target: "/api/v1/probe/key", header: map[string]string{"Authorization": "Bearer " + k.Key}}); rec.Code != http.StatusOK {
		t.Fatalf("alice's key stopped working after mallory's attempt: %d", rec.Code)
	}
	if len(listKeys(t, e, alice)) != 1 {
		t.Fatal("alice's key disappeared")
	}
}

func TestAPIKeyRoutesNeedSession(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	k := createKey(t, e, b, "editor")
	// An API key can't manage API keys.
	rec := e.do(req{target: "/api/v1/api_keys", header: map[string]string{"Authorization": "Bearer " + k.Key}})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("list with API key = %d, want 401", rec.Code)
	}
}

func TestFailedAPIKeyAttemptsAreRateLimited(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	k := createKey(t, e, b, "editor")
	ip := "192.0.2.50:1"
	bad := map[string]string{"Authorization": "Bearer 3f1e2d4c-1111-4222-8333-444455556666"}
	limited := false
	for range 200 {
		rec := e.do(req{target: "/api/v1/probe/key", header: bad, remoteAddr: ip})
		if rec.Code == http.StatusTooManyRequests {
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			limited = true
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad key = %d, want 401 or 429", rec.Code)
		}
	}
	if !limited {
		t.Fatal("200 bad keys from one IP were never limited")
	}
	// A neighbour behind the same NAT with a valid key is not blocked by
	// someone else's failures.
	good := map[string]string{"Authorization": "Bearer " + k.Key}
	for range 5 {
		if rec := e.do(req{target: "/api/v1/probe/key", header: good, remoteAddr: ip}); rec.Code != http.StatusOK {
			t.Fatalf("valid key from limited IP = %d, want 200", rec.Code)
		}
	}
	// Other IPs keep their own failure budget.
	if rec := e.do(req{target: "/api/v1/probe/key", header: bad, remoteAddr: "192.0.2.51:1"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key from other IP = %d, want 401", rec.Code)
	}
}

func TestFailedAPIKeyLimitGroupsIPv6ByPrefix(t *testing.T) {
	e := newEnv(t)
	bad := map[string]string{"Authorization": "Bearer 3f1e2d4c-1111-4222-8333-444455556666"}
	// Rotating through addresses in one /64 doesn't buy a fresh budget.
	limited := false
	for i := range 200 {
		addr := fmt.Sprintf("[2001:db8:5:6::%x]:1", i+1)
		if rec := e.do(req{target: "/api/v1/probe/key", header: bad, remoteAddr: addr}); rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("bad keys from 200 addresses in one /64 were never limited")
	}
}

func TestSuccessfulAPIKeyRequestsDontTouchFailureLimiter(t *testing.T) {
	e := newEnv(t)
	b := e.login(fiona)
	k := createKey(t, e, b, "editor")
	good := map[string]string{"Authorization": "Bearer " + k.Key}
	for i := range 50 {
		if rec := e.do(req{target: "/api/v1/probe/key", header: good, remoteAddr: fmt.Sprintf("198.51.100.%d:1", i+1)}); rec.Code != http.StatusOK {
			t.Fatalf("good key = %d", rec.Code)
		}
	}
	if n := auth.KeyFailBuckets(e.svc); n != 0 {
		t.Fatalf("failure limiter tracks %d clients after only successes, want 0", n)
	}
}
