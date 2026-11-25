package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Rails merges the body under the query (request_parameters.merge(
// query_parameters)): a key sent in both is the query's.
func TestReadParamsQueryWins(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, contentType, body string
	}{
		{"JSON", "application/json", `{"email": "b@example.com", "query": "from body"}`},
		{"form", "application/x-www-form-urlencoded", `email=b%40example.com&query=from+body`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/admin/v1/user/get_user_by_email?email=a@example.com", strings.NewReader(c.body))
			r.Header.Set("Content-Type", c.contentType)
			p, err := readParams(r)
			if err != nil {
				t.Fatal(err)
			}
			if p["email"] != "a@example.com" || p["query"] != "from body" {
				t.Fatalf("params = %v", p)
			}
		})
	}
}

// include_ai is cast as ActiveModel::Type::Boolean casts it, whose false
// values are case-sensitive: "False" and "Off" are true.
func TestBoolParamIsRailsBoolean(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"": false, "0": false, "f": false, "F": false, "false": false, "FALSE": false, "off": false, "OFF": false,
		"False": true, "Off": true, "fALSE": true, "oFF": true, "1": true, "true": true, "no": true, " ": true, "0 ": true,
	} {
		if got := (params{"include_ai": v}).boolParam("include_ai"); got != want {
			t.Errorf("include_ai=%q = %v, want %v", v, got, want)
		}
	}
	if (params{}).boolParam("include_ai") {
		t.Error("a missing include_ai is true")
	}
}
